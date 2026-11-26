// Package svc implements the lifecycle verbs: --install, --uninstall and
// the apt post-invoke repair. Every mutating step is idempotent, guarded
// by an advisory lock, and validated (perl -c) before Proxmox services
// are restarted. A legacy proxmox-rmem install is detected and migrated
// away automatically during install.
package svc

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Wu-Wu/pve-real-memory/internal/config"
	"github.com/Wu-Wu/pve-real-memory/internal/patch"
	"github.com/Wu-Wu/pve-real-memory/internal/paths"
	"github.com/Wu-Wu/pve-real-memory/internal/system"
)

// Layout is the full set of filesystem locations; overridable for tests.
type Layout struct {
	BinaryPath      string // path referenced from the unit/apt hook
	ConfigPath      string // config file the unit's ExecStart passes
	PMFile          string // QemuServer.pm
	UnitFile        string
	AptHookFile     string
	LockFile        string
	ConfigDir       string
	ConfigFile      string
	StateDir        string
	PatchBackupDir  string
	LegacyBackupDir string
	SSHKeyPath      string
	OverrideDir     string

	Legacy struct {
		UnitFile     string
		AptHookFile  string
		AptHelper    string
		Script       string
		PatchScript  string
		ConfigDir    string
		PMBackup     string
		AutopatchLog string
		OverrideGlob string
	}
}

// DefaultLayout uses the production paths.
func DefaultLayout() *Layout {
	bin := paths.BinaryDefault
	if self, err := os.Executable(); err == nil && self != paths.BinaryDefault {
		// Running from an upload location (e.g. /root): reference the
		// real file so a bare `--install` works before the binary is
		// moved into place.
		bin = self
	}
	l := &Layout{
		BinaryPath:      bin,
		ConfigPath:      paths.ConfigFile,
		PMFile:          pmFile(),
		UnitFile:        paths.UnitFile,
		AptHookFile:     paths.AptHookFile,
		LockFile:        paths.LockFile,
		ConfigDir:       paths.ConfigDir,
		ConfigFile:      paths.ConfigFile,
		StateDir:        paths.StateDir,
		PatchBackupDir:  paths.PatchBackupDir,
		LegacyBackupDir: paths.LegacyBackupDir,
		SSHKeyPath:      paths.SSHKeyDefault,
		OverrideDir:     paths.OverrideDir,
	}
	l.Legacy.UnitFile = paths.LegacyUnitFile
	l.Legacy.AptHookFile = paths.LegacyAptHookFile
	l.Legacy.AptHelper = paths.LegacyAptHelper
	l.Legacy.Script = paths.LegacyScript
	l.Legacy.PatchScript = paths.LegacyPatchScript
	l.Legacy.ConfigDir = paths.LegacyConfigDir
	l.Legacy.PMBackup = paths.LegacyBackupPM
	l.Legacy.AutopatchLog = paths.LegacyAutopatchLog
	l.Legacy.OverrideGlob = paths.LegacyOverrideGlob
	return l
}

func pmFile() string {
	if v := os.Getenv("PVE_QEMUSERVER_PM"); v != "" {
		return v
	}
	return paths.QemuServerPM
}

// Ops wires the lifecycle to the outside world; overridable for tests.
type Ops struct {
	Layout *Layout
	Out    io.Writer // human progress
	// Run runs a host command (systemctl); tests inject a recorder.
	Run func(name string, args ...string) error
}

func (o *Ops) logf(format string, args ...any) {
	if o.Out != nil {
		fmt.Fprintf(o.Out, format+"\n", args...)
	}
}

func (o *Ops) run(name string, args ...string) error {
	if o.Run != nil {
		return o.Run(name, args...)
	}
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (o *Ops) engine() *patch.Engine {
	return &patch.Engine{
		Path:        o.Layout.PMFile,
		OverrideDir: o.Layout.OverrideDir,
		TTLSeconds:  config.PatchTTLSeconds,
		Verifier:    system.PerlCompile,
		BackupFunc: func(original []byte) error {
			if o.Layout.PatchBackupDir == "" {
				return nil
			}
			if err := os.MkdirAll(o.Layout.PatchBackupDir, 0o700); err != nil {
				return err
			}
			name := fmt.Sprintf("QemuServer.pm.%s", time.Now().Format("20060102-150405"))
			return os.WriteFile(filepath.Join(o.Layout.PatchBackupDir, name), original, 0o600)
		},
	}
}

// requireRoot refuses lifecycle ops for non-root callers (the unit, the
// apt hook and the patched module must all end up owned by root).
func requireRoot() error {
	if os.Geteuid() != 0 && os.Getenv("PRM_SKIP_ROOT_CHECK") == "" {
		return errors.New("install/uninstall/repair must run as root")
	}
	return nil
}

// Install sets up the whole layer: patch, config skeleton, ssh key,
// systemd unit, apt post-invoke hook, services.
func Install(o *Ops) error {
	if err := requireRoot(); err != nil {
		return err
	}
	lock, err := system.AcquireLock(o.Layout.LockFile)
	if err != nil {
		return err
	}
	defer lock.Release()

	if _, err := os.Stat(o.Layout.PMFile); err != nil {
		return fmt.Errorf("%s not found — this host does not look like a Proxmox node (override with PVE_QEMUSERVER_PM)", o.Layout.PMFile)
	}

	o.migrateLegacy()

	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{
		{o.Layout.ConfigDir, 0o755},
		{o.Layout.StateDir, 0o755},
		{o.Layout.PatchBackupDir, 0o700},
		{o.Layout.OverrideDir, 0o700},
	} {
		if err := os.MkdirAll(dir.path, dir.mode); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir.path, err)
		}
	}

	// config
	if _, err := os.Stat(o.Layout.ConfigFile); err != nil {
		if err := os.WriteFile(o.Layout.ConfigFile, []byte(ConfigTemplate), 0o644); err != nil {
			return err
		}
		o.logf("wrote %s (edit to taste; changes are picked up live)", o.Layout.ConfigFile)
	}

	// ssh key
	if pub, created, err := EnsureKey(o.Layout.SSHKeyPath); err != nil {
		return err
	} else if created {
		o.logf("generated monitoring key %s", o.Layout.SSHKeyPath)
		o.logf("authorize it on SSH-monitored guests (os: ssh sources):")
		o.logf("    mkdir -p ~/.ssh && echo '%s' >> ~/.ssh/authorized_keys", strings.TrimSpace(pub))
	}

	// patch
	applied, fallback, err := o.engine().Apply()
	if err != nil {
		return fmt.Errorf("applying vmstatus patch: %w", err)
	}
	switch {
	case fallback:
		o.logf("WARNING: vmstatus patch applied via fallback anchor; review %s", o.Layout.PMFile)
	case applied:
		o.logf("vmstatus patch applied (verified with perl -c, backup under %s)", o.Layout.PatchBackupDir)
	default:
		o.logf("vmstatus patch already applied")
	}

	// unit + apt hook
	if err := os.MkdirAll(filepath.Dir(o.Layout.UnitFile), 0o755); err != nil {
		return err
	}
	unitArgs := "run"
	if o.Layout.ConfigPath != "" && o.Layout.ConfigPath != paths.ConfigFile {
		unitArgs = "run --config " + o.Layout.ConfigPath
	}
	if err := os.WriteFile(o.Layout.UnitFile, []byte(UnitFile(o.Layout.BinaryPath, unitArgs)), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(o.Layout.AptHookFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(o.Layout.AptHookFile, []byte(AptHook(o.Layout.BinaryPath)), 0o644); err != nil {
		return err
	}
	o.logf("wrote %s and %s", o.Layout.UnitFile, o.Layout.AptHookFile)

	if err := o.run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := o.run("systemctl", "enable", "--now", paths.ServiceName); err != nil {
		return err
	}
	// Reload the perl services so the patch takes effect now, not on
	// the next pvestatd restart.
	if err := o.run("systemctl", "try-restart", "--no-block", "pvestatd", "pvedaemon"); err != nil {
		o.logf("WARNING: %v (restart pvestatd/pvedaemon manually)", err)
	}
	o.logf("installed. check: systemctl status %s && journalctl -u %s -f", paths.ServiceName, paths.ServiceName)
	return nil
}

// Uninstall reverses the install. The config dir and known_hosts are
// kept unless purge=true (they may hold credentials references an
// operator curated).
func Uninstall(o *Ops, purge bool) error {
	if err := requireRoot(); err != nil {
		return err
	}
	lock, err := system.AcquireLock(o.Layout.LockFile)
	if err != nil {
		return err
	}
	defer lock.Release()

	// Order matters: stop the feeder before removing the consumer.
	_ = o.run("systemctl", "disable", "--now", paths.ServiceName)
	_ = o.run("systemctl", "stop", paths.ServiceName)

	if err := removeIfExists(o.Layout.UnitFile, o); err != nil {
		return err
	}
	if err := removeIfExists(o.Layout.AptHookFile, o); err != nil {
		return err
	}
	if err := o.run("systemctl", "daemon-reload"); err != nil {
		o.logf("WARNING: daemon-reload: %v", err)
	}

	removed, err := o.engine().Remove()
	if err != nil {
		return fmt.Errorf("removing vmstatus patch: %w", err)
	}
	if removed {
		o.logf("vmstatus patch removed (perl -c verified)")
		_ = o.run("systemctl", "try-restart", "--no-block", "pvestatd", "pvedaemon")
	} else {
		o.logf("vmstatus patch was not present")
	}

	if err := os.RemoveAll(o.Layout.OverrideDir); err != nil {
		o.logf("WARNING: remove %s: %v", o.Layout.OverrideDir, err)
	}

	if purge {
		for _, p := range []string{o.Layout.ConfigDir, o.Layout.StateDir} {
			if err := os.RemoveAll(p); err != nil {
				o.logf("WARNING: remove %s: %v", p, err)
			} else {
				o.logf("removed %s", p)
			}
		}
	} else {
		o.logf("kept %s and %s (use --purge to remove)", o.Layout.ConfigDir, o.Layout.StateDir)
	}
	o.logf("uninstalled")
	return nil
}

// Repair re-applies the patch when it went missing (Proxmox package
// upgrade overwrote QemuServer.pm). In hook mode it never fails loudly:
// apt must not break because of us.
func Repair(o *Ops, hook bool) error {
	if err := requireRoot(); err != nil {
		return err
	}
	lock, err := system.AcquireLock(o.Layout.LockFile)
	if err != nil {
		if hook {
			o.logf("repair skipped: %v", err)
			return nil
		}
		return err
	}
	defer lock.Release()

	st := o.engine().Status()
	switch st {
	case patch.Applied:
		if !hook {
			o.logf("patch applied")
		}
		return nil
	case patch.FileMissing:
		if hook {
			return nil // not a PVE host update we care about
		}
		return fmt.Errorf("%s missing", o.Layout.PMFile)
	}

	o.logf("patch missing (%s), re-applying", st)
	applied, fallback, err := o.engine().Apply()
	if err != nil {
		if hook {
			o.logf("repair FAILED (apt continues unaffected): %v", err)
			return nil
		}
		return err
	}
	if applied && fallback {
		o.logf("WARNING: applied via fallback anchor; review the diff")
	}
	if applied {
		if err := o.run("systemctl", "try-restart", "--no-block", "pvestatd", "pvedaemon"); err != nil {
			o.logf("WARNING: %v", err)
		}
		if !hook {
			o.logf("patch re-applied, pvestatd/pvedaemon restart requested")
		}
	}
	return nil
}

// StatusText renders `status` output.
func StatusText(o *Ops) string {
	st := o.engine().Status()
	var b strings.Builder
	fmt.Fprintf(&b, "vmstatus patch: %s\n", st)
	if _, err := os.Stat(o.Layout.UnitFile); err == nil {
		unit := "disabled"
		if out, err := exec.Command("systemctl", "is-active", paths.ServiceName).Output(); err == nil {
			unit = strings.TrimSpace(string(out))
		}
		fmt.Fprintf(&b, "service: unit installed, %s\n", unit)
	} else {
		fmt.Fprintf(&b, "service: not installed\n")
	}
	ents, _ := os.ReadDir(o.Layout.OverrideDir)
	entries := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".mem") {
			entries++
		}
	}
	fmt.Fprintf(&b, "overrides: %d in %s\n", entries, o.Layout.OverrideDir)
	if _, err := os.Stat(o.Layout.AptHookFile); err == nil {
		fmt.Fprintf(&b, "apt hook: installed\n")
	} else {
		fmt.Fprintf(&b, "apt hook: not installed\n")
	}
	return b.String()
}

// EnsureKey generates an ed25519 monitoring key pair when missing.
// Returns the authorized_keys line.
func EnsureKey(path string) (pub string, created bool, err error) {
	if _, err := os.Stat(path); err == nil {
		raw, err := os.ReadFile(path + ".pub")
		return string(raw), false, err
	}
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", false, err
	}
	block, err := ssh.MarshalPrivateKey(privKey, "pve-real-memory monitoring key")
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", false, err
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return "", false, err
	}
	line := string(ssh.MarshalAuthorizedKey(sshPub))
	if err := os.WriteFile(path+".pub", []byte(line), 0o644); err != nil {
		return "", false, err
	}
	return line, true, nil
}

// migrateLegacy tears down a proxmox-rmem install: daemon, apt hook,
// scripts, its patch block and its stray files — everything moved to a
// timestamped backup dir, nothing silently destroyed.
func (o *Ops) migrateLegacy() {
	detected := false
	for _, p := range []string{o.Layout.Legacy.UnitFile, o.Layout.Legacy.Script, o.Layout.Legacy.ConfigDir, o.Layout.Legacy.AptHelper, o.Layout.Legacy.PatchScript} {
		if _, err := os.Stat(p); err == nil {
			detected = true
			break
		}
	}
	if globHas(o.Layout.Legacy.OverrideGlob) {
		detected = true
	}
	if !detected {
		return
	}
	o.logf("legacy proxmox-rmem install detected — migrating")

	stamp := time.Now().Format("20060102-150405")
	dst := filepath.Join(o.Layout.LegacyBackupDir, stamp)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		o.logf("WARNING: legacy backup dir: %v", err)
		return
	}

	// Stop the old feeder BEFORE removing anything it may rewrite.
	_ = o.run("systemctl", "disable", "--now", "proxmox-rmem")
	_ = o.run("systemctl", "stop", "proxmox-rmem")

	// Remove the legacy patch block from the live module (the .bak must
	// NOT be restored: it would downgrade a newer PVE file).
	removed, err := o.engine().RemoveLegacy()
	if err != nil {
		o.logf("WARNING: legacy patch removal: %v", err)
	} else if removed {
		o.logf("legacy patch block removed from %s", o.Layout.PMFile)
	}

	moved := 0
	move := func(src string) {
		if src == "" {
			return
		}
		if strings.Contains(src, "*") {
			for _, m := range glob(src) {
				o.moveFile(m, dst)
				moved++
			}
			return
		}
		if _, err := os.Stat(src); err == nil {
			o.moveFile(src, dst)
			moved++
		}
	}
	move(o.Layout.Legacy.UnitFile)
	move(o.Layout.Legacy.AptHookFile)
	move(o.Layout.Legacy.AptHelper)
	move(o.Layout.Legacy.Script)
	move(o.Layout.Legacy.PatchScript)
	move(o.Layout.Legacy.ConfigDir)
	move(o.Layout.Legacy.PMBackup)
	move(o.Layout.Legacy.AutopatchLog)
	move(o.Layout.Legacy.OverrideGlob)

	if _, err := os.Stat(o.Layout.PMFile); err == nil {
		if err := system.PerlCompile(o.Layout.PMFile); err != nil {
			o.logf("WARNING: %s no longer compiles after legacy cleanup: %v", o.Layout.PMFile, err)
		}
	}
	_ = o.run("systemctl", "daemon-reload")
	o.logf("legacy files moved to %s (%d entries)", dst, moved)
}

func (o *Ops) moveFile(src, dstDir string) {
	dst := filepath.Join(dstDir, filepath.Base(filepath.Dir(src))+"__"+filepath.Base(src))
	if err := os.Rename(src, dst); err != nil {
		// cross-device or permission: fall back to copy+remove
		if err := copyRemove(src, dst); err != nil {
			o.logf("WARNING: move %s: %v", src, err)
		}
	}
}

func copyRemove(src, dst string) error {
	if fi, err := os.Stat(src); err != nil {
		return err
	} else if fi.IsDir() {
		return errors.New("dir copy unsupported; move manually")
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return err
	}
	return os.Remove(src)
}

func globHas(pattern string) bool { return len(glob(pattern)) > 0 }

func glob(pattern string) []string {
	m, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	return m
}

func removeIfExists(path string, o *Ops) error {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	o.logf("removed %s", path)
	return nil
}
