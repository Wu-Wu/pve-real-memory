package memsrc

import "testing"

func TestParseLinuxMeminfo(t *testing.T) {
	sample := `MemTotal:        4000000 kB
MemFree:          500000 kB
MemAvailable:    3000000 kB
Buffers:          100000 kB
Cached:           800000 kB
`
	// used = (4000000-3000000) kB = 1024000000 bytes
	got, err := ParseLinuxMeminfo(sample)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(1000000 * 1024); got != want {
		t.Errorf("got %d want %d", got, want)
	}
}

func TestParseLinuxMeminfoZero(t *testing.T) {
	// Fully idle node: MemAvailable == MemTotal, used is legitimately 0.
	sample := "MemTotal: 1000 kB\nMemAvailable: 1000 kB\n"
	got, err := ParseLinuxMeminfo(sample)
	if err != nil || got != 0 {
		t.Fatalf("got %d, %v", got, err)
	}
}

func TestParseLinuxMeminfoErrors(t *testing.T) {
	if _, err := ParseLinuxMeminfo("MemTotal: 100 kB\n"); err == nil {
		t.Error("missing MemAvailable must error")
	}
	if _, err := ParseLinuxMeminfo("MemTotal: 100 kB\nMemAvailable: 200 kB\n"); err == nil {
		t.Error("avail > total must error")
	}
}

func TestParseFreeBSDSysctl(t *testing.T) {
	// 100000 active + 50000 wired pages, 4 KiB pages
	got, err := ParseFreeBSDSysctl("100000\n50000\n4096\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(150000 * 4096); got != want {
		t.Errorf("got %d want %d", got, want)
	}
	if _, err := ParseFreeBSDSysctl("100000 50000"); err == nil {
		t.Error("short output must error")
	}
	if _, err := ParseFreeBSDSysctl("a b c"); err == nil {
		t.Error("non-numeric must error")
	}
}

func TestParseWindowsCIM(t *testing.T) {
	// numbers or strings both accepted; kB in, bytes out
	got, err := ParseWindowsCIM(`{"TotalVisibleMemorySize":8388608,"FreePhysicalMemory":4194304}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(4194304 * 1024); got != want {
		t.Errorf("got %d want %d", got, want)
	}
	got, err = ParseWindowsCIM(`{"TotalVisibleMemorySize":"8388608","FreePhysicalMemory":"4194304"}`)
	if err != nil || got != int64(4194304*1024) {
		t.Errorf("string form: got %d, %v", got, err)
	}
	if _, err := ParseWindowsCIM("garbage"); err == nil {
		t.Error("non-JSON must error")
	}
}

func TestParseMeminfoJSONProtocol(t *testing.T) {
	// real meminfo-service payload
	got, err := ParseMeminfoJSON(`{"total":33552644096,"avail":29835171840,"used":3717472256}`)
	if err != nil || got != 3717472256 {
		t.Fatalf("got %d, %v", got, err)
	}
	// total/avail only, including the fully-loaded avail=0 edge
	got, err = ParseMeminfoJSON(`{"total":1000,"avail":0}`)
	if err != nil || got != 1000 {
		t.Fatalf("avail=0 edge: got %d, %v", got, err)
	}
	// used=0 idle node
	got, err = ParseMeminfoJSON(`{"total":1000,"avail":1000,"used":0}`)
	if err != nil || got != 0 {
		t.Fatalf("idle node: got %d, %v", got, err)
	}
	// stringified numbers
	got, err = ParseMeminfoJSON(`{"total":"1000","avail":"400"}`)
	if err != nil || got != 600 {
		t.Fatalf("strings: got %d, %v", got, err)
	}
	// float used is NOT accepted (would be truncated silently)
	if _, err := ParseMeminfoJSON(`{"used":12.5}`); err == nil {
		t.Error("float must be rejected, not truncated")
	}
	// negatives rejected
	if _, err := ParseMeminfoJSON(`{"used":-1}`); err == nil {
		t.Error("negative must be rejected")
	}
	// avail > total rejected
	if _, err := ParseMeminfoJSON(`{"total":10,"avail":20}`); err == nil {
		t.Error("avail>total must be rejected")
	}
	// wrong shapes
	for _, bad := range []string{`[1,2]`, `"hello"`, `123`, `{}`, ``} {
		if _, err := ParseMeminfoJSON(bad); err == nil {
			t.Errorf("must reject %q", bad)
		}
	}
}

func TestSSHCommands(t *testing.T) {
	if s, err := SSHCommand("linux"); err != nil || s != "cat /proc/meminfo" {
		t.Errorf("linux ssh cmd = %q, %v", s, err)
	}
	if _, err := SSHCommand("windows"); err == nil {
		t.Error("windows over ssh must be refused")
	}
}
