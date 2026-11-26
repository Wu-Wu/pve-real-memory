// Package rmemconf backs up and restores the legacy proxmox-rmem
// configuration (config.json). It is the whole job of the
// proxmox-rmem-helper binary: pve-real-memory's installer deliberately
// leaves the legacy config in place, so saving it before a migration
// and putting it back for a rollback is this helper's responsibility.
//
// Both operations are pure file copies: missing source (config for
// --backup, backup for --restore) means "nothing to do", never an
// error. All user-facing text is produced here so the caller only has
// to print it to stdout.
package rmemconf

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Wu-Wu/pve-real-memory/internal/paths"
)

// Paths are the two files the helper works on; overridable for tests.
type Paths struct {
	ConfigFile string // /etc/proxmox-rmem/config.json
	BackupFile string // /var/backups/proxmox-rmem/config.json
}

// Default returns the production paths.
func Default() Paths {
	return Paths{
		ConfigFile: paths.LegacyConfigFile,
		BackupFile: paths.RmemBackupFile,
	}
}

// Backup copies the current config to the backup path (overwriting any
// previous backup). noop is true when there is no config to save.
func (p Paths) Backup() (msg string, noop bool, err error) {
	fi, err := os.Stat(p.ConfigFile)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("no proxmox-rmem configuration found at %s — nothing to do", p.ConfigFile), true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("stat %s: %w", p.ConfigFile, err)
	}
	if fi.IsDir() {
		return "", false, fmt.Errorf("%s is a directory, not a config file", p.ConfigFile)
	}

	data, mode, err := readCopy(p.ConfigFile, fi)
	if err != nil {
		return "", false, err
	}

	replaced := fileExists(p.BackupFile)
	if err := atomicWrite(p.BackupFile, data, mode); err != nil {
		return "", false, err
	}

	msg = fmt.Sprintf("backup: saved %s (%d bytes, mode %04o, sha256 %s)\n        -> %s",
		p.ConfigFile, len(data), mode, digest(data), p.BackupFile)
	if replaced {
		msg += " (previous backup replaced)"
	}
	return msg, false, nil
}

// Restore copies the backup back over the config, creating the config
// directory when needed (e.g. after the old package was purged).
// noop is true when there is no backup to restore.
func (p Paths) Restore() (msg string, noop bool, err error) {
	fi, err := os.Stat(p.BackupFile)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("no proxmox-rmem backup found at %s — nothing to do", p.BackupFile), true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("stat %s: %w", p.BackupFile, err)
	}
	if fi.IsDir() {
		return "", false, fmt.Errorf("%s is a directory, not a backup file", p.BackupFile)
	}

	data, mode, err := readCopy(p.BackupFile, fi)
	if err != nil {
		return "", false, err
	}

	hadCurrent := fileExists(p.ConfigFile)
	if err := atomicWrite(p.ConfigFile, data, mode); err != nil {
		return "", false, err
	}

	msg = fmt.Sprintf("restore: restored %s (%d bytes, mode %04o, sha256 %s)\n        -> %s",
		p.BackupFile, len(data), mode, digest(data), p.ConfigFile)
	if hadCurrent {
		msg += " (existing config overwritten)"
	}
	msg += "\n        restart the proxmox-rmem service to apply"
	return msg, false, nil
}

// readCopy loads a file and reports its permission bits.
func readCopy(name string, fi fs.FileInfo) ([]byte, fs.FileMode, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", name, err)
	}
	mode := fi.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	return data, mode, nil
}

// atomicWrite writes via a sibling temp file + rename so an interrupted
// run can never leave a half-written config behind.
func atomicWrite(name string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(name)+".tmp")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, name); err != nil {
		return fmt.Errorf("rename -> %s: %w", name, err)
	}
	return nil
}

func fileExists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
