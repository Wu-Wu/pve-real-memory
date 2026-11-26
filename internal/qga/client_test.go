package qga

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAgent mimics qemu-guest-agent JSONL framing, including the
// guest-sync-delimited 0xFF sentinel and stale-byte emission.
type fakeAgent struct {
	t         *testing.T
	dir       string
	stale     string // bytes emitted before the first sync reply
	closeFast bool   // immediately close every connection
}

func newFakeAgent(t *testing.T, opts *fakeAgent) *fakeAgent {
	t.Helper()
	dir, err := os.MkdirTemp("", "prm-qga-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	opts.dir = dir
	ln, err := net.Listen("unix", filepath.Join(dir, "7.qga"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go opts.handle(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return opts
}

func (f *fakeAgent) handle(c net.Conn) {
	defer c.Close()
	if f.closeFast {
		return
	}
	w := bufio.NewWriter(c)
	r := bufio.NewReader(c)
	sentStale := false
	for {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		var req struct {
			Execute   string          `json:"execute"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		switch req.Execute {
		case "guest-sync-delimited":
			if !sentStale && f.stale != "" {
				w.WriteString(f.stale)
				sentStale = true
			}
			var args struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(req.Arguments, &args)
			fmt.Fprintf(w, "\xff{\"return\": %d}\n", args.ID)
		case "guest-exec":
			fmt.Fprint(w, "{\"return\": {\"pid\": 4242}}\n")
		case "guest-exec-status":
			out := base64.StdEncoding.EncodeToString([]byte("MemTotal: 4000000 kB\n"))
			fmt.Fprintf(w, "{\"return\": {\"exitcode\": 0, \"exited\": true, \"out-data\": %q}}\n", out)
		case "guest-get-osinfo":
			fmt.Fprint(w, "{\"return\": {\"name\": \"Debian GNU Bookworm 12\", \"id\": \"debian\", \"kernel-release\": \"6.1.0\"}}\n")
		case "guest-get-network-interfaces":
			fmt.Fprint(w, "{\"return\": [{\"name\": \"lo\", \"ip-addresses\": [{\"ip-address\": \"127.0.0.1\", \"ip-address-type\": \"ipv4\"}]}, {\"name\": \"eth0\", \"ip-addresses\": [{\"ip-address\": \"10.0.0.5\", \"ip-address-type\": \"ipv4\"}, {\"ip-address\": \"fe80::1\", \"ip-address-type\": \"ipv6\"}]}]}\n")
		case "must-fail":
			fmt.Fprint(w, "{\"error\": {\"class\": \"GenericError\", \"desc\": \"boom\"}}\n")
		default:
			fmt.Fprintf(w, "{\"error\": {\"class\": \"CommandNotFound\", \"desc\": \"unknown\"}}\n")
		}
		w.Flush()
	}
}

func (f *fakeAgent) runner() *Runner {
	r := NewRunner(f.dir, 3*time.Second)
	r.PollInterval = 10 * time.Millisecond
	return r
}

func TestExecHappyPath(t *testing.T) {
	f := newFakeAgent(t, &fakeAgent{})
	out, err := f.runner().Exec(7, "cat", []string{"/proc/meminfo"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "MemTotal") {
		t.Errorf("out = %q", out)
	}
}

func TestStaleBytesAreDiscarded(t *testing.T) {
	// The agent leaks an old response and an event before our sync.
	f := newFakeAgent(t, &fakeAgent{stale: "{\"return\": {\"stale\": \"ancient response\"}}\n{\"event\": \"BLOCK_IO_ERROR\"}\n"})
	out, err := f.runner().Exec(7, "cat", []string{"/proc/meminfo"})
	if err != nil {
		t.Fatalf("stale bytes must be discarded via resync: %v", err)
	}
	if !strings.Contains(out, "MemTotal") {
		t.Errorf("out = %q", out)
	}
}

func TestOSInfo(t *testing.T) {
	f := newFakeAgent(t, &fakeAgent{})
	info, err := f.runner().GetOSInfo(7)
	if err != nil {
		t.Fatal(err)
	}
	if info.Detect() != "linux" {
		t.Errorf("Detect = %q", info.Detect())
	}
}

func TestCommandErrorSurfaces(t *testing.T) {
	f := newFakeAgent(t, &fakeAgent{})
	_, err := f.runner().Call(7, "must-fail", nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v", err)
	}
}

func TestSocketHeldByOtherClient(t *testing.T) {
	f := newFakeAgent(t, &fakeAgent{closeFast: true})
	_, err := f.runner().Exec(7, "cat", nil)
	if err == nil {
		t.Fatal("must fail when agent closes immediately")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error should explain the collision: %v", err)
	}
}

func TestDetectOS(t *testing.T) {
	cases := []struct {
		info OSInfo
		want string
	}{
		{OSInfo{ID: "opnsense", Name: "OPNsense"}, "freebsd"},
		{OSInfo{ID: "talos", Name: "Talos"}, "talos"},
		{OSInfo{ID: "debian", Name: "Debian GNU/Linux 12"}, "linux"},
		{OSInfo{Name: "Microsoft Windows 11 Pro", ID: "windows"}, "windows"},
		{OSInfo{Name: "FreeBSD", KernelRelease: "14.0-RELEASE"}, "freebsd"},
		{OSInfo{}, ""},
	}
	for _, c := range cases {
		if got := c.info.Detect(); got != c.want {
			t.Errorf("Detect(%+v) = %q, want %q", c.info, got, c.want)
		}
	}
}

func TestGetIPv4(t *testing.T) {
	f := newFakeAgent(t, &fakeAgent{})
	ip, err := f.runner().GetIPv4(7)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.0.0.5" {
		t.Errorf("ip = %q, want 10.0.0.5 (skip lo and ipv6)", ip)
	}
}

func TestListVMIDsAndRunning(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"101.qga", "202.qga", "notes.txt", "x.qga"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := ListVMIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != 101 || ids[1] != 202 {
		t.Errorf("ids = %v", ids)
	}
	if Running(dir, 101) {
		t.Error("regular file is not a socket")
	}
}
