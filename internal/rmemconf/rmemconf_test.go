package rmemconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	return Paths{
		ConfigFile: filepath.Join(root, "etc", "proxmox-rmem", "config.json"),
		BackupFile: filepath.Join(root, "var", "backups", "proxmox-rmem", "config.json"),
	}
}

func mustWrite(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupSaveRestoreRoundTrip(t *testing.T) {
	p := testPaths(t)
	const cfg = `{"auto": true, "vms": [101]}`
	mustWrite(t, p.ConfigFile, cfg)

	msg, noop, err := p.Backup()
	if err != nil || noop {
		t.Fatalf("backup: noop=%v err=%v", noop, err)
	}
	if !strings.Contains(msg, "backup: saved") || !strings.Contains(msg, "sha256") {
		t.Errorf("unexpected success message: %s", msg)
	}
	bak, err := os.ReadFile(p.BackupFile)
	if err != nil || string(bak) != cfg {
		t.Fatalf("backup content = %q, err=%v", bak, err)
	}
	fi, _ := os.Stat(p.BackupFile)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("backup must keep source mode, got %04o", fi.Mode().Perm())
	}

	// Simulate a purge of the old package: config dir gone entirely.
	if err := os.RemoveAll(filepath.Dir(p.ConfigFile)); err != nil {
		t.Fatal(err)
	}
	msg, noop, err = p.Restore()
	if err != nil || noop {
		t.Fatalf("restore: noop=%v err=%v", noop, err)
	}
	back, err := os.ReadFile(p.ConfigFile)
	if err != nil || string(back) != cfg {
		t.Fatalf("restored content = %q, err=%v", back, err)
	}
	if !strings.Contains(msg, "restore: restored") || !strings.Contains(msg, "restart the proxmox-rmem service") {
		t.Errorf("unexpected restore message: %s", msg)
	}
	fi, _ = os.Stat(p.ConfigFile)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("restored mode = %04o, want 0600", fi.Mode().Perm())
	}
}

func TestBackupMissingConfigIsNoop(t *testing.T) {
	p := testPaths(t)
	msg, noop, err := p.Backup()
	if err != nil || !noop {
		t.Fatalf("missing config must be a no-op, got noop=%v err=%v", noop, err)
	}
	if !strings.Contains(msg, "no proxmox-rmem configuration found") || !strings.Contains(msg, p.ConfigFile) {
		t.Errorf("noop message must name the path: %s", msg)
	}
	if _, err := os.Stat(p.BackupFile); !os.IsNotExist(err) {
		t.Error("no-op backup must not create anything")
	}
}

func TestRestoreMissingBackupIsNoop(t *testing.T) {
	p := testPaths(t)
	msg, noop, err := p.Restore()
	if err != nil || !noop {
		t.Fatalf("missing backup must be a no-op, got noop=%v err=%v", noop, err)
	}
	if !strings.Contains(msg, "no proxmox-rmem backup found") || !strings.Contains(msg, p.BackupFile) {
		t.Errorf("noop message must name the path: %s", msg)
	}
	if _, err := os.Stat(filepath.Dir(p.ConfigFile)); !os.IsNotExist(err) {
		t.Error("no-op restore must not create the config dir")
	}
}

func TestBackupReplacesAndRestoreOverwritesAnnounce(t *testing.T) {
	p := testPaths(t)
	mustWrite(t, p.ConfigFile, "one")
	if _, _, err := p.Backup(); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, p.ConfigFile, "two")
	msg, _, err := p.Backup()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "previous backup replaced") {
		t.Errorf("replacement must be announced: %s", msg)
	}
	mustWrite(t, p.ConfigFile, "current") // a config newer than the backup
	msg, _, err = p.Restore()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "existing config overwritten") {
		t.Errorf("overwrite must be announced: %s", msg)
	}
	got, _ := os.ReadFile(p.ConfigFile)
	if string(got) != "two" {
		t.Errorf("restore must bring back the backuped bytes, got %q", got)
	}
}

// A write interrupted mid-way must never leave a partial config behind.
func TestAtomicWriteLeavesNothingPartial(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	mustWrite(t, target, "original")
	// atomicWrite failing before rename (bad dir perms are hard to force
	// as root in CI; instead assert the success path leaves no temp junk)
	if err := atomicWrite(target, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("temp files must not survive: %v", ents)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "new" {
		t.Errorf("content = %q", got)
	}
}
