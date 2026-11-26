// Package rmemconf backs up and restores the legacy proxmox-rmem
// configuration — the whole /etc/proxmox-rmem directory (config.json,
// the id_rsa_monitor key whose loss would mean redeploying
// authorized_keys on every guest, and the install stamp). It is the
// whole job of the proxmox-rmem-helper binary: pve-real-memory's
// installer deliberately leaves the directory in place, so saving it
// before a migration and putting it back for a rollback is this
// helper's responsibility.
//
// What is deliberately NOT here: the PVE patch ("врезка") and
// QemuServer.pm .bak snapshots. Copying a whole Perl module over a
// possibly-newer installed file is the exact downgrade defect of the
// old installer; re-applying a patch is the respective project's
// installer job, not a config restore.
//
// A backup is one tar.gz holding the directory's regular files with
// their permission bits. A missing source (config dir for --backup,
// archive for --restore) means "nothing to do", never an error. All
// user-facing text is produced here so the caller only has to print it
// to stdout.
package rmemconf

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Wu-Wu/pve-real-memory/internal/paths"
)

// Paths are the two locations the helper works on; overridable for tests.
type Paths struct {
	ConfigDir  string // /etc/proxmox-rmem
	BackupFile string // /var/backups/proxmox-rmem/etc-proxmox-rmem.tar.gz
}

// Default returns the production paths.
func Default() Paths {
	return Paths{
		ConfigDir:  paths.LegacyConfigDir,
		BackupFile: paths.RmemBackupFile,
	}
}

// Backup tars the whole config directory into the backup file
// (overwriting any previous backup). noop is true when there is no
// config directory or it holds no regular files.
func (p Paths) Backup() (msg string, noop bool, err error) {
	fi, err := os.Stat(p.ConfigDir)
	if errors.Is(err, fs.ErrNotExist) {
		return p.noConfigMsg(), true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("stat %s: %w", p.ConfigDir, err)
	}
	if !fi.IsDir() {
		return "", false, fmt.Errorf("%s is not a directory", p.ConfigDir)
	}

	var files []string // relative names, sorted for a deterministic archive
	err = filepath.WalkDir(p.ConfigDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil // sockets/symlinks have no place in a config backup
		}
		rel, rerr := filepath.Rel(p.ConfigDir, path)
		if rerr != nil {
			return rerr
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return "", false, fmt.Errorf("scan %s: %w", p.ConfigDir, err)
	}
	if len(files) == 0 {
		return p.noConfigMsg(), true, nil
	}
	sort.Strings(files)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	var total int64
	for _, rel := range files {
		full := filepath.Join(p.ConfigDir, rel)
		info, serr := os.Stat(full)
		if serr != nil {
			return "", false, serr
		}
		data, serr := os.ReadFile(full)
		if serr != nil {
			return "", false, serr
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     filepath.ToSlash(rel),
			Mode:     int64(info.Mode().Perm()),
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
			ModTime:  info.ModTime(),
		}); err != nil {
			return "", false, err
		}
		if _, err := tw.Write(data); err != nil {
			return "", false, err
		}
		total += int64(len(data))
	}
	if err := tw.Close(); err != nil {
		return "", false, err
	}
	if err := zw.Close(); err != nil { // flushes the gzip footer
		return "", false, err
	}
	archive := buf.Bytes()

	replaced := fileExists(p.BackupFile)
	if err := atomicWrite(p.BackupFile, archive, 0o600); err != nil {
		return "", false, err
	}

	msg = fmt.Sprintf("backup: saved %d files (%d bytes) from %s\n        -> %s (mode 0600, sha256 %s)",
		len(files), total, p.ConfigDir, p.BackupFile, digest(archive))
	if replaced {
		msg += " (previous backup replaced)"
	}
	return msg, false, nil
}

// Restore unpacks the archive into the config directory, creating it
// when needed (e.g. after the old package was purged). Existing files
// are overwritten, modes are the saved ones. noop is true when there
// is no backup.
func (p Paths) Restore() (msg string, noop bool, err error) {
	archive, err := os.ReadFile(p.BackupFile)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("no proxmox-rmem backup found at %s — nothing to do", p.BackupFile), true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", p.BackupFile, err)
	}

	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", false, fmt.Errorf("%s is not a valid backup archive: %w", p.BackupFile, err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	var restored, hadAny int
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", false, fmt.Errorf("read archive %s: %w", p.BackupFile, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		rel := filepath.FromSlash(hdr.Name)
		if rel == "" || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", false, fmt.Errorf("refusing unsafe entry %q in %s", hdr.Name, p.BackupFile)
		}
		full := filepath.Join(p.ConfigDir, rel)
		if fileExists(full) {
			hadAny++
		}
		data, rerr := io.ReadAll(io.LimitReader(tr, 16<<20)) // config files are tiny; cap hostile archives
		if rerr != nil {
			return "", false, rerr
		}
		mode := fs.FileMode(hdr.Mode).Perm()
		if mode == 0 {
			mode = 0o600
		}
		if werr := atomicWrite(full, data, mode); werr != nil {
			return "", false, werr
		}
		restored++
	}
	if restored == 0 {
		return "", false, fmt.Errorf("backup %s contains no files", p.BackupFile)
	}

	msg = fmt.Sprintf("restore: restored %d files from %s\n        -> %s",
		restored, p.BackupFile, p.ConfigDir)
	if hadAny > 0 {
		msg += " (existing files overwritten)"
	}
	msg += "\n        restart the proxmox-rmem service to apply"
	return msg, false, nil
}

func (p Paths) noConfigMsg() string {
	return fmt.Sprintf("no proxmox-rmem configuration found at %s — nothing to do", p.ConfigDir)
}

// atomicWrite writes via a sibling temp file + rename so an interrupted
// run can never leave a half-written file behind.
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
