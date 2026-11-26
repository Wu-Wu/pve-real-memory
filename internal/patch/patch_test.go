package patch

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(raw)
}

func newTestEngine(t *testing.T, content string, verifier Verifier) *Engine {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "QemuServer.pm")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Engine{Path: path, OverrideDir: "/run/pve-real-memory", TTLSeconds: 120, Verifier: verifier}
}

var fixtures = []string{"vmstatus-9.2.pl", "vmstatus-9.3.pl"}

func TestApplyInsertsAfterAnchorBeforeReturn(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			orig := fixture(t, name)
			e := newTestEngine(t, orig, func(string) error { return nil })

			applied, fb, err := e.Apply()
			if err != nil || !applied || fb {
				t.Fatalf("Apply = (%v, %v, %v)", applied, fb, err)
			}
			if e.Status() != Applied {
				t.Fatal("Status after Apply")
			}

			raw, _ := os.ReadFile(e.Path)
			got := string(raw)

			ai := strings.Index(got, anchorPrimary)
			bi := strings.Index(got, BeginMarker)
			ri := strings.LastIndex(got, "return $res;")
			if !(ai < bi && bi < ri) {
				t.Fatalf("order wrong: anchor=%d begin=%d return=%d", ai, bi, ri)
			}
			// Indent must match the return line's indent.
			line := got[strings.LastIndex(got[:bi], "\n")+1:]
			if !strings.HasPrefix(line, "    "+BeginMarker) {
				t.Errorf("bad indent: %q", line[:40])
			}
			// The rest of the file must be untouched around insertion.
			if !strings.HasPrefix(got, orig[:ai]) {
				t.Error("prefix changed")
			}
		})
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	orig := fixture(t, "vmstatus-9.3.pl")
	e := newTestEngine(t, orig, func(string) error { return nil })
	if a, _, err := e.Apply(); err != nil || !a {
		t.Fatal("first apply failed")
	}
	raw1, _ := os.ReadFile(e.Path)
	if a, _, err := e.Apply(); err != nil || a {
		t.Fatalf("second apply should no-op: (%v, %v)", a, err)
	}
	raw2, _ := os.ReadFile(e.Path)
	if string(raw1) != string(raw2) {
		t.Error("second apply modified the file")
	}
}

func TestRemoveRestoresOriginal(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			orig := fixture(t, name)
			e := newTestEngine(t, orig, func(string) error { return nil })
			if _, _, err := e.Apply(); err != nil {
				t.Fatal(err)
			}
			removed, err := e.Remove()
			if err != nil || !removed {
				t.Fatalf("Remove = (%v, %v)", removed, err)
			}
			raw, _ := os.ReadFile(e.Path)
			if string(raw) != orig {
				t.Errorf("not byte-identical to original after remove:\n--- got tail ---\n%s",
					gotAroundDiff(orig, string(raw)))
			}
			if removed, err := e.Remove(); removed || err != nil {
				t.Errorf("second Remove must be a no-op: (%v, %v)", removed, err)
			}
		})
	}
}

func gotAroundDiff(a, b string) string {
	la := strings.Split(a, "\n")
	lb := strings.Split(b, "\n")
	for i := range la {
		if i >= len(lb) || la[i] != lb[i] {
			lo := max(0, i-2)
			return strings.Join(lb[lo:min(i+3, len(lb))], "\n")
		}
	}
	return "(no diff)"
}

func TestVerifierFailureReverts(t *testing.T) {
	orig := fixture(t, "vmstatus-9.3.pl")
	e := newTestEngine(t, orig, func(string) error { return errors.New("syntax error at line 42") })
	if _, _, err := e.Apply(); err == nil {
		t.Fatal("Apply must fail when verifier fails")
	} else if !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("error = %v", err)
	}
	raw, _ := os.ReadFile(e.Path)
	if string(raw) != orig {
		t.Error("file not reverted after failed verification")
	}
}

func TestLegacyRemoval(t *testing.T) {
	orig := fixture(t, "vmstatus-9.3.pl")
	// Synthesize the old project's generated block (new shape, from its
	// patch_pve.py _build_patch_code with the vmstatus indent of 4 spaces).
	legacy := "\n    # proxmox-rmem: Override memory from external file if available\n" +
		"    foreach my $vmid (keys %$res) {\n" +
		"        if (-f \"/tmp/pve-vm-$vmid-mem-override\") {\n" +
		"            if (open(my $fh, '<', \"/tmp/pve-vm-$vmid-mem-override\")) {\n" +
		"                my $override_mem = <$fh>;\n" +
		"                chomp $override_mem;\n" +
		"                if ($override_mem && $override_mem =~ /^\\d+$/) {\n" +
		"                    $res->{$vmid}->{mem} = $override_mem;\n" +
		"                }\n" +
		"                close($fh);\n" +
		"            }\n" +
		"        }\n" +
		"    }\n\n"
	pos, indent, _, err := findInsertion(orig)
	if err != nil {
		t.Fatal(err)
	}
	if indent != "    " {
		t.Fatalf("fixture indent = %q", indent)
	}
	patched := orig[:pos] + legacy + orig[pos:]

	e := newTestEngine(t, patched, func(string) error { return nil })
	if !e.LegacyPresent() {
		t.Fatal("LegacyPresent")
	}
	removed, err := e.RemoveLegacy()
	if err != nil || !removed {
		t.Fatalf("RemoveLegacy = (%v, %v)", removed, err)
	}
	raw, _ := os.ReadFile(e.Path)
	if string(raw) != orig {
		t.Errorf("legacy removal not byte-clean:\n%s", gotAroundDiff(orig, string(raw)))
	}
}

func TestFallbackInsertionWithoutAnchor(t *testing.T) {
	orig := fixture(t, "vmstatus-9.3.pl")
	stripped := strings.Replace(orig, "    "+anchorPrimary+"\n", "", 1)
	if stripped == orig {
		t.Fatal("anchor not stripped")
	}
	e := newTestEngine(t, stripped, func(string) error { return nil })
	applied, fb, err := e.Apply()
	if err != nil || !applied || !fb {
		t.Fatalf("Apply = (%v, %v, %v)", applied, fb, err)
	}
	raw, _ := os.ReadFile(e.Path)
	got := string(raw)
	if strings.Index(got, BeginMarker) > strings.LastIndex(got, "return $res;") {
		t.Error("fallback inserted after return")
	}
}

func TestRefusesUnexpectedFile(t *testing.T) {
	e := newTestEngine(t, "package PVE::QemuServer;\nsomething entirely different;\n", nil)
	if _, _, err := e.Apply(); err == nil {
		t.Fatal("must refuse to patch a file with neither anchor nor vmstatus")
	}
}

func TestBlockCompilesWithPerlIfAvailable(t *testing.T) {
	// On CI/macOS perl exists but cannot compile PVE deps; only check the
	// generated block for Perl syntax via a standalone snippet.
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("no perl")
	}
	block := Block("    ", 120, "/tmp/prm-test")
	// Wrap in a sub with a fake $res to compile.
	src := "sub vmstatus {\n    my $res = shift;\n" + block + "    return $res;\n}\n"
	tmp := filepath.Join(t.TempDir(), "snippet.pl")
	if err := os.WriteFile(tmp, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(perl, "-c", tmp).CombinedOutput()
	if err != nil {
		t.Fatalf("generated perl block does not compile: %v\n%s", err, out)
	}
}
