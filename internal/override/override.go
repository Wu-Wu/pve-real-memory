// Package override maintains the per-VM memory override files consumed by
// the vmstatus() hook. Files live in a root-only directory under /run
// (tmpfs, mode 0700): unlike the legacy /tmp layout they are immune to
// symlink and squatting attacks from unprivileged local users.
package override

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Store writes <dir>/<vmid>.mem files atomically.
type Store struct {
	Dir string
}

// New prepares the store directory (0700, root).
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create override dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

// Path returns the override file for a VM.
func (s *Store) Path(vmid int) string {
	return filepath.Join(s.Dir, strconv.Itoa(vmid)+".mem")
}

// Write atomically replaces the override with usedBytes (decimal bytes).
// A reader (pvestatd) never observes a truncated file: either the previous
// value or the new one.
func (s *Store) Write(vmid int, usedBytes int64) error {
	if usedBytes < 0 {
		return fmt.Errorf("negative memory value for vmid %d", vmid)
	}
	tmp := filepath.Join(s.Dir, fmt.Sprintf(".tmp-%d", vmid))
	content := []byte(strconv.FormatInt(usedBytes, 10) + "\n")
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path(vmid))
}

// Delete removes the override file if present.
func (s *Store) Delete(vmid int) error {
	err := os.Remove(s.Path(vmid))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Prune removes overrides for VMIDs not in active. It only touches files
// named <digits>.mem.
func (s *Store) Prune(active map[int]bool) ([]int, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var removed []int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".mem") {
			continue
		}
		vmid, err := strconv.Atoi(strings.TrimSuffix(name, ".mem"))
		if err != nil {
			continue // not ours
		}
		if active[vmid] {
			continue
		}
		if err := os.Remove(filepath.Join(s.Dir, name)); err == nil {
			removed = append(removed, vmid)
		}
	}
	return removed, nil
}

// Read exposes the current value (diagnostics).
func (s *Store) Read(vmid int) (int64, error) {
	raw, err := os.ReadFile(s.Path(vmid))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
}
