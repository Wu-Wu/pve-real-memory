package override

import (
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "ovr"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWriteRead(t *testing.T) {
	s := newStore(t)
	if err := s.Write(101, 1234567); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(101)
	if err != nil || got != 1234567 {
		t.Fatalf("Read = %d, %v", got, err)
	}
	fi, err := os.Stat(s.Path(101))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("override mode = %o, want 600", fi.Mode().Perm())
	}
	dirfi, _ := os.Stat(s.Dir)
	if dirfi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 700", dirfi.Mode().Perm())
	}
	if err := s.Write(101, -5); err == nil {
		t.Error("negative value must be rejected")
	}
}

func TestPruneKeepsActive(t *testing.T) {
	s := newStore(t)
	for _, v := range []int{101, 202, 303} {
		if err := s.Write(v, 1); err != nil {
			t.Fatal(err)
		}
	}
	// foreign file must survive
	os.WriteFile(filepath.Join(s.Dir, "not-ours.txt"), []byte("x"), 0o600)

	removed, err := s.Prune(map[int]bool{101: true, 303: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != 202 {
		t.Errorf("removed = %v, want [202]", removed)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "not-ours.txt")); err != nil {
		t.Error("foreign file must not be touched")
	}
	if _, err := s.Read(101); err != nil {
		t.Error("active override must survive")
	}
}

func TestDeleteMissingIsOK(t *testing.T) {
	s := newStore(t)
	if err := s.Delete(999); err != nil {
		t.Errorf("delete of missing file: %v", err)
	}
}
