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
		ConfigDir:  filepath.Join(root, "etc", "proxmox-rmem"),
		BackupFile: filepath.Join(root, "var", "backups", "proxmox-rmem", "etc-proxmox-rmem.tar.gz"),
	}
}

func mustWrite(t *testing.T, name, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestBackupSaveRestoreRoundTrip(t *testing.T) {
	p := testPaths(t)
	mustWrite(t, filepath.Join(p.ConfigDir, "config.json"), `{"auto": true}`, 0o600)
	mustWrite(t, filepath.Join(p.ConfigDir, "id_rsa_monitor"), "PRIVATE KEY MATERIAL", 0o600)
	mustWrite(t, filepath.Join(p.ConfigDir, "id_rsa_monitor.pub"), "ssh-rsa AAAA monitor@pve", 0o644)
	mustWrite(t, filepath.Join(p.ConfigDir, ".installed_commit"), "de785c0\n", 0o644)

	msg, noop, err := p.Backup()
	if err != nil || noop {
		t.Fatalf("backup: noop=%v err=%v", noop, err)
	}
	if !strings.Contains(msg, "saved 4 files") || !strings.Contains(msg, "sha256") {
		t.Errorf("unexpected success message: %s", msg)
	}
	bfi, err := os.Stat(p.BackupFile)
	if err != nil || bfi.Mode().Perm() != 0o600 {
		t.Errorf("archive must exist with mode 0600 (it contains private keys), err=%v", err)
	}

	// Simulate a purge of the old package: config dir gone entirely.
	if err := os.RemoveAll(p.ConfigDir); err != nil {
		t.Fatal(err)
	}
	msg, noop, err = p.Restore()
	if err != nil || noop {
		t.Fatalf("restore: noop=%v err=%v", noop, err)
	}
	if !strings.Contains(msg, "restored 4 files") || !strings.Contains(msg, "restart the proxmox-rmem service") {
		t.Errorf("unexpected restore message: %s", msg)
	}
	wants := []struct {
		name    string
		content string
		mode    os.FileMode
	}{
		{"config.json", `{"auto": true}`, 0o600},
		{"id_rsa_monitor", "PRIVATE KEY MATERIAL", 0o600},
		{"id_rsa_monitor.pub", "ssh-rsa AAAA monitor@pve", 0o644},
		{".installed_commit", "de785c0\n", 0o644},
	}
	for _, w := range wants {
		full := filepath.Join(p.ConfigDir, w.name)
		data, err := os.ReadFile(full)
		if err != nil || string(data) != w.content {
			t.Errorf("%s content = %q, err=%v", w.name, data, err)
		}
		fi, _ := os.Stat(full)
		if fi.Mode().Perm() != w.mode {
			t.Errorf("%s mode = %04o, want %04o", w.name, fi.Mode().Perm(), w.mode)
		}
	}
}

func TestBackupMissingConfigIsNoop(t *testing.T) {
	p := testPaths(t)
	msg, noop, err := p.Backup()
	if err != nil || !noop {
		t.Fatalf("missing config must be a no-op, got noop=%v err=%v", noop, err)
	}
	if !strings.Contains(msg, "no proxmox-rmem configuration found") || !strings.Contains(msg, p.ConfigDir) {
		t.Errorf("noop message must name the path: %s", msg)
	}
	if _, err := os.Stat(p.BackupFile); !os.IsNotExist(err) {
		t.Error("no-op backup must not create anything")
	}

	// Existing but empty dir is still "nothing to do".
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, noop, err := p.Backup(); err != nil || !noop {
		t.Errorf("empty dir must be a no-op, got noop=%v err=%v", noop, err)
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
	if _, err := os.Stat(p.ConfigDir); !os.IsNotExist(err) {
		t.Error("no-op restore must not create the config dir")
	}
}

func TestBackupReplacesAndRestoreOverwritesAnnounce(t *testing.T) {
	p := testPaths(t)
	mustWrite(t, filepath.Join(p.ConfigDir, "config.json"), "one", 0o644)
	if _, _, err := p.Backup(); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(p.ConfigDir, "config.json"), "two", 0o644)
	msg, _, err := p.Backup()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "previous backup replaced") {
		t.Errorf("replacement must be announced: %s", msg)
	}
	mustWrite(t, filepath.Join(p.ConfigDir, "config.json"), "current", 0o644) // newer than the backup
	msg, _, err = p.Restore()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "existing files overwritten") {
		t.Errorf("overwrite must be announced: %s", msg)
	}
	got, _ := os.ReadFile(filepath.Join(p.ConfigDir, "config.json"))
	if string(got) != "two" {
		t.Errorf("restore must bring back the backuped bytes, got %q", got)
	}
}

func TestRestoreRejectsGarbageArchive(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(filepath.Dir(p.BackupFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.BackupFile, []byte("not gzip at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Restore(); err == nil || !strings.Contains(err.Error(), "not a valid backup archive") {
		t.Errorf("garbage archive must fail clearly, got %v", err)
	}
}

// Atomic writes must never leave temp junk next to restored files.
func TestAtomicWriteLeavesNothingPartial(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	mustWrite(t, target, "original", 0o644)
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
