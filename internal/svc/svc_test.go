package svc

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/Wu-Wu/pve-real-memory/internal/patch"
	"github.com/Wu-Wu/pve-real-memory/internal/system"
)

const pmFixture = `package PVE::QemuServer;

sub vmstatus {
    my ($cache) = @_;
    my $res = {};
    my $qmpclient = PVE::QMPClient->new();
    $qmpclient->queue_execute(undef, 2);

    return $res;
}
1;
`

func testOps(t *testing.T) (*Ops, *Layout, *[]string, *bytes.Buffer) {
	t.Helper()
	t.Setenv("PRM_SKIP_ROOT_CHECK", "1")
	root := t.TempDir()
	l := &Layout{
		BinaryPath:      filepath.Join(root, "pve-real-memory"),
		PMFile:          filepath.Join(root, "QemuServer.pm"),
		UnitFile:        filepath.Join(root, "unit", "pve-real-memory.service"),
		AptHookFile:     filepath.Join(root, "apt", "99-pve-real-memory"),
		LockFile:        filepath.Join(root, "lock"),
		ConfigDir:       filepath.Join(root, "etc"),
		ConfigFile:      filepath.Join(root, "etc", "config.yaml"),
		StateDir:        filepath.Join(root, "lib"),
		PatchBackupDir:  filepath.Join(root, "lib", "patch-backups"),
		LegacyBackupDir: filepath.Join(root, "lib", "legacy-backup"),
		SSHKeyPath:      filepath.Join(root, "etc", "id_ed25519"),
		OverrideDir:     filepath.Join(root, "run"),
	}
	l.Legacy.UnitFile = filepath.Join(root, "unit", "proxmox-rmem.service")
	l.Legacy.AptHookFile = filepath.Join(root, "apt", "99-proxmox-rmem")
	l.Legacy.AptHelper = filepath.Join(root, "bin", "proxmox-rmem-apt-hook.sh")
	l.Legacy.Script = filepath.Join(root, "bin", "proxmox-rmem.py")
	l.Legacy.PatchScript = filepath.Join(root, "bin", "proxmox-rmem-patch.py")
	l.Legacy.ConfigDir = filepath.Join(root, "etc-legacy")
	l.Legacy.PMBackup = l.PMFile + ".bak"
	l.Legacy.AutopatchLog = filepath.Join(root, "log")
	l.Legacy.OverrideGlob = filepath.Join(root, "tmp", "pve-vm-*-mem-override")

	if err := os.MkdirAll(filepath.Dir(l.PMFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.PMFile, []byte(pmFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var cmds []string
	out := &bytes.Buffer{}
	o := &Ops{
		Layout: l,
		Out:    out,
		Run: func(name string, args ...string) error {
			cmds = append(cmds, name+" "+strings.Join(args, " "))
			return nil
		},
	}
	return o, l, &cmds, out
}

func TestInstallAndUninstallLifecycle(t *testing.T) {
	o, l, cmds, _ := testOps(t)

	if err := Install(o); err != nil {
		t.Fatalf("install: %v", err)
	}

	// patch applied to the live file
	raw, _ := os.ReadFile(l.PMFile)
	if !strings.Contains(string(raw), patch.BeginMarker) {
		t.Error("patch not applied")
	}
	// config + key + unit + hook
	cfgRaw, err := os.ReadFile(l.ConfigFile)
	if err != nil || !bytes.Contains(cfgRaw, []byte("autoDiscovery")) {
		t.Error("config template not written")
	}
	if _, err := os.Stat(l.SSHKeyPath); err != nil {
		t.Error("ssh key not generated")
	}
	if fi, _ := os.Stat(l.SSHKeyPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode = %o", fi.Mode().Perm())
	}
	unit, _ := os.ReadFile(l.UnitFile)
	if !strings.Contains(string(unit), "ExecStart="+l.BinaryPath+" run") ||
		!strings.Contains(string(unit), "RuntimeDirectory=pve-real-memory") {
		t.Errorf("unit wrong:\n%s", unit)
	}
	hook, _ := os.ReadFile(l.AptHookFile)
	if !strings.Contains(string(hook), l.BinaryPath+" repair --hook") {
		t.Errorf("apt hook wrong:\n%s", hook)
	}
	joined := strings.Join(*cmds, "\n")
	for _, want := range []string{"daemon-reload", "enable --now pve-real-memory", "try-restart --no-block pvestatd pvedaemon"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing systemctl call %q in:\n%s", want, joined)
		}
	}

	// backups exist
	ents, _ := os.ReadDir(l.PatchBackupDir)
	if len(ents) == 0 {
		t.Error("no QemuServer.pm backup taken")
	}

	// second install is a no-op-ish: same content, still applied
	before, _ := os.ReadFile(l.PMFile)
	if err := Install(o); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	after, _ := os.ReadFile(l.PMFile)
	if !bytes.Equal(before, after) {
		t.Error("reinstall must not duplicate the patch block")
	}

	// uninstall restores the original file content exactly
	if err := Uninstall(o, false); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	raw, _ = os.ReadFile(l.PMFile)
	if string(raw) != pmFixture {
		t.Errorf("PM not restored:\n%s", raw)
	}
	if _, err := os.Stat(l.UnitFile); !os.IsNotExist(err) {
		t.Error("unit not removed")
	}
	if _, err := os.Stat(l.ConfigFile); err != nil {
		t.Error("config must survive uninstall without --purge")
	}
}

func TestPurgeRemovesState(t *testing.T) {
	o, l, _, _ := testOps(t)
	if err := Install(o); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(o, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.ConfigDir); !os.IsNotExist(err) {
		t.Error("--purge must remove config dir")
	}
	if _, err := os.Stat(l.StateDir); !os.IsNotExist(err) {
		t.Error("--purge must remove state dir")
	}
}

func TestInstallMigratesLegacy(t *testing.T) {
	o, l, cmds, out := testOps(t)

	// Lay down a full legacy footprint, incl. its patch block in the PM.
	os.MkdirAll(filepath.Dir(l.Legacy.Script), 0o755)
	os.MkdirAll(l.Legacy.ConfigDir, 0o755)
	os.MkdirAll(filepath.Dir(l.Legacy.OverrideGlob), 0o755)
	for _, f := range []string{l.Legacy.UnitFile, l.Legacy.AptHookFile, l.Legacy.AptHelper,
		l.Legacy.Script, l.Legacy.PatchScript, l.Legacy.AutopatchLog,
		filepath.Join(l.Legacy.ConfigDir, "config.json")} {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("legacy stuff\n"), 0o644)
	}
	os.WriteFile(filepath.Join(filepath.Dir(l.Legacy.OverrideGlob), "pve-vm-55-mem-override"), []byte("123"), 0o644)

	legacyPM := strings.Replace(pmFixture, "\n    return $res;", `
    # proxmox-rmem: Override memory from external file if available
    foreach my $vmid (keys %$res) {
        my $file = "/tmp/pve-vm-$vmid-mem-override";
    }

    return $res;`, 1)
	os.WriteFile(l.PMFile, []byte(legacyPM), 0o644)
	// And the dreaded .bak from the old installer:
	os.WriteFile(l.Legacy.PMBackup, []byte("ancient PVE file from before the upgrade\n"), 0o644)

	if err := Install(o); err != nil {
		t.Fatalf("install with legacy: %v", err)
	}

	logged := out.String()
	if !strings.Contains(logged, "legacy proxmox-rmem install detected") {
		t.Errorf("expected migration notice, got:\n%s", logged)
	}
	raw, _ := os.ReadFile(l.PMFile)
	if strings.Contains(string(raw), "proxmox-rmem") {
		t.Error("legacy patch block must be gone")
	}
	if !strings.Contains(string(raw), patch.BeginMarker) {
		t.Error("new patch must be applied")
	}
	for _, f := range []string{l.Legacy.UnitFile, l.Legacy.Script,
		l.Legacy.PMBackup, l.Legacy.AutopatchLog,
		filepath.Join(filepath.Dir(l.Legacy.OverrideGlob), "pve-vm-55-mem-override")} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("legacy file still present: %s", f)
		}
	}
	// the config dir survives: backups are proxmox-rmem-helper's job
	if cfg := filepath.Join(l.Legacy.ConfigDir, "config.json"); true {
		b, err := os.ReadFile(cfg)
		if err != nil || string(b) != "legacy stuff\n" {
			t.Errorf("legacy config must stay untouched at %s (err=%v)", cfg, err)
		}
	}
	// .bak must NOT have been restored over the live file
	if strings.Contains(string(raw), "ancient PVE file") {
		t.Error("legacy .bak must be archived, never restored over the newer file")
	}
	// backup archive contains the moved files
	var moved int
	filepath.WalkDir(l.LegacyBackupDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			moved++
		}
		return nil
	})
	if moved < 6 {
		t.Errorf("only %d files archived", moved)
	}
	joined := strings.Join(*cmds, "\n")
	if !strings.Contains(joined, "stop proxmox-rmem") {
		t.Errorf("legacy service must be stopped:\n%s", joined)
	}
}

func TestRepair(t *testing.T) {
	o, l, _, _ := testOps(t)
	if err := Install(o); err != nil {
		t.Fatal(err)
	}

	// simulate apt overwrite: file without our block
	if err := os.WriteFile(l.PMFile, []byte(pmFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Repair(o, false); err != nil {
		t.Fatalf("repair: %v", err)
	}
	raw, _ := os.ReadFile(l.PMFile)
	if !strings.Contains(string(raw), patch.BeginMarker) {
		t.Error("repair did not re-apply")
	}

	// hook mode on a vanished file: must not error (apt safety)
	os.Remove(l.PMFile)
	if err := Repair(o, true); err != nil {
		t.Errorf("hook repair must be silent-safe: %v", err)
	}
}

func TestRepairNeverBreaksAptWhenPerlBroken(t *testing.T) {
	o, l, _, out := testOps(t)
	// A module where our block would not compile: anchor inside broken perl.
	broken := "package PVE::QemuServer;\nsub vmstatus { $qmpclient->queue_execute(undef, 2);\n  this is ((( not perl\n}\n"
	os.WriteFile(l.PMFile, []byte(broken), 0o644)

	if err := Repair(o, true); err != nil {
		t.Errorf("hook mode must swallow the failure: %v", err)
	}
	// the broken file must be left exactly as apt installed it (revert)
	raw, _ := os.ReadFile(l.PMFile)
	if strings.Contains(string(raw), patch.BeginMarker) {
		t.Error("failed verification must revert the file")
	}
	if !strings.Contains(out.String(), "repair FAILED") {
		t.Log(out.String())
	}
}

func TestEnsureKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "id_ed25519")

	pub, created, err := EnsureKey(path)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if _, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(pub)); err != nil || len(rest) > 0 {
		t.Errorf("authorized_keys line must parse cleanly: err=%v rest=%q", err, rest)
	}
	if _, err := ssh.ParsePrivateKey(mustRead(t, path)); err != nil {
		t.Errorf("private key unparseable: %v", err)
	}
	pub2, created2, err := EnsureKey(path)
	if err != nil || created2 || pub2 != pub {
		t.Errorf("second call: created=%v equal=%v err=%v", created2, pub2 == pub, err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUnitAndHookRender(t *testing.T) {
	if !strings.Contains(UnitFile("/opt/x/prm", "run"), "ExecStart=/opt/x/prm run") {
		t.Error("unit binary path")
	}
	if !strings.Contains(AptHook("/opt/x/prm"), `"/opt/x/prm repair --hook";`) {
		t.Error("hook binary path")
	}
}

func TestLockSerialization(t *testing.T) {
	o, _, _, _ := testOps(t)
	lock, err := system.AcquireLock(o.Layout.LockFile)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := Install(o); err == nil || !strings.Contains(err.Error(), "holds") {
		t.Errorf("install must refuse while locked: %v", err)
	}
}
