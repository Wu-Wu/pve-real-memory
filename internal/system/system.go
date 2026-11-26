// Package system wraps the few host facilities the binary needs: perl
// syntax checking, systemd unit control, and an advisory lock that
// serializes mutating operations.
package system

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// PerlCompile runs `perl -c` over a module file and reports failure with
// perl's own message.
func PerlCompile(path string) error {
	perl, err := exec.LookPath("perl")
	if err != nil {
		return fmt.Errorf("perl not found: %w", err)
	}
	out, err := exec.Command(perl, "-c", path).CombinedOutput()
	text := string(out)
	if err != nil {
		return fmt.Errorf("perl -c: %s: %w", strings.TrimSpace(text), err)
	}
	if !strings.Contains(text, "syntax OK") {
		return fmt.Errorf("perl -c unexpected output: %s", strings.TrimSpace(text))
	}
	return nil
}

// RestartUnits performs systemctl try-restart --no-block (never fails on
// missing units, never blocks the caller on job completion — important in
// apt post-invoke context).
func RestartUnits(units ...string) error {
	if len(units) == 0 {
		return nil
	}
	args := append([]string{"try-restart", "--no-block"}, units...)
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(units, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Systemctl runs an arbitrary systemctl verb (install/uninstall paths).
func Systemctl(args ...string) (string, error) {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// AptHookUnits are the Proxmox services that cache QemuServer.pm in RAM
// and must reload after patch apply/removal.
var PVERestartUnits = []string{"pvestatd", "pvedaemon"}

// Lock is an advisory exclusive lock serializing install/uninstall/repair
// and daemon patch operations.
type Lock struct {
	f *os.File
}

// AcquireLock takes a non-blocking exclusive flock.
func AcquireLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another pve-real-memory operation holds %s", path)
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	l.f.Close()
}
