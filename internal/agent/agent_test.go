package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wu-Wu/pve-real-memory/internal/httpc"
	"github.com/Wu-Wu/pve-real-memory/internal/patch"
	"github.com/Wu-Wu/pve-real-memory/internal/qga"
	"github.com/Wu-Wu/pve-real-memory/internal/sshc"
)

func testAgent(t *testing.T, cfgYAML string) (*Agent, string) {
	t.Helper()
	dir := t.TempDir()
	ovr := filepath.Join(dir, "ovr")
	cfgPath := filepath.Join(dir, "config.yaml")
	body := strings.ReplaceAll(cfgYAML, "@OVR@", ovr)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		CfgPath: cfgPath,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		state:   map[int]*vmState{},
		disc:    map[int]discEntry{},
	}
	var err error
	a.cfg, a.cfgMod, err = a.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	a.applyConfig()
	a.RestartPVE = func() error { return nil }
	a.PatchOK = func() patch.Status { return patch.Applied }
	return a, ovr
}

func TestCycleHTTPAndPrune(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]int{"total": 1000, "avail": 600, "used": 400})
	}))
	defer srv.Close()

	a, ovr := testAgent(t, `
interval: 1s
overrideDir: "@OVR@"
sources:
  - vmid: 303
    method: http
    url: `+srv.URL+`/meminfo
`)
	a.HTTPFetch = httpc.Fetch

	a.cycle(context.Background())

	raw, err := os.ReadFile(filepath.Join(ovr, "303.mem"))
	if err != nil {
		t.Fatalf("override missing: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "400" {
		t.Errorf("override = %q", raw)
	}

	// Remove the source from config -> next cycle must prune the file.
	cfgPath := a.CfgPath
	os.WriteFile(cfgPath, []byte("interval: 1s\noverrideDir: \""+ovr+"\"\n"), 0o644)
	time.Sleep(10 * time.Millisecond)
	a.reloadIfChanged()
	a.cycle(context.Background())
	if _, err := os.Stat(filepath.Join(ovr, "303.mem")); !os.IsNotExist(err) {
		t.Error("override must be pruned when the source disappears")
	}
}

func TestFailureKeepsOverrideForTTL(t *testing.T) {
	a, ovr := testAgent(t, `
interval: 1s
overrideDir: "@OVR@"
sources:
  - vmid: 303
    method: http
    url: http://127.0.0.1:1/nope
`)
	a.HTTPFetch = httpc.Fetch

	a.cycle(context.Background()) // fails
	// file absent (never succeeded) — fine.
	a.HTTPFetch = func(httpc.Options) (string, error) { return `{"used": 7}`, nil }
	a.cycle(context.Background()) // succeeds
	if _, err := os.Stat(filepath.Join(ovr, "303.mem")); err != nil {
		t.Fatal("good value must be recorded")
	}
	a.HTTPFetch = func(httpc.Options) (string, error) { return "", errors.New("boom") }
	a.cycle(context.Background()) // fails again -> stale file ages out on its own
	if _, err := os.Stat(filepath.Join(ovr, "303.mem")); err != nil {
		t.Error("transient failure must NOT delete the override (TTL expiry handles it)")
	}
}

func TestQGAAutoDetectionFlow(t *testing.T) {
	a, ovr := testAgent(t, `
interval: 1s
overrideDir: "@OVR@"
autoDiscovery:
  enabled: true
qga:
  socketDir: "@SOCK@"
`)
	// fake socket + fake agent identity/exec
	sockDir := filepath.Join(t.TempDir(), "qs")
	os.MkdirAll(sockDir, 0o755)
	os.WriteFile(filepath.Join(sockDir, "555.qga"), nil, 0o600)
	a.cfg.QGA.SocketDir = sockDir
	a.discAt = time.Time{} // force discovery

	a.QGAOSInfo = func(vmid int) (*qga.OSInfo, error) {
		return &qga.OSInfo{Name: "Debian GNU Linux 12", ID: "debian"}, nil
	}
	a.QGAExec = func(vmid int, path string, args []string) (string, error) {
		if path == "cat" {
			return "MemTotal: 100 kB\nMemAvailable: 40 kB\n", nil
		}
		return "", errors.New("unexpected exec " + path)
	}

	a.cycle(context.Background())

	if _, ok := a.disc[555]; !ok {
		t.Fatal("vm 555 must be auto-discovered")
	}
	raw, err := os.ReadFile(filepath.Join(ovr, "555.mem"))
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "61440" { // (100-40)kB
		t.Errorf("override = %q", raw)
	}

	// socket disappears -> vm is dropped from discovery
	os.Remove(filepath.Join(sockDir, "555.qga"))
	a.discAt = time.Time{}
	a.cycle(context.Background())
	if _, ok := a.disc[555]; ok {
		t.Error("vm without socket must be dropped")
	}
}

func TestExplicitBeatsAutoAndExclude(t *testing.T) {
	a, _ := testAgent(t, `
interval: 1s
sources:
  - vmid: 101
    method: qga
autoDiscovery:
  enabled: true
  exclude: [999]
`)
	a.disc[101] = discEntry{os: "linux", added: time.Now()}
	a.disc[999] = discEntry{os: "linux", added: time.Now()}
	a.disc[202] = discEntry{os: "freebsd", added: time.Now()}

	got := a.Targets()
	if len(got) != 2 {
		t.Fatalf("targets = %+v", got)
	}
	if got[0].VMID != 101 || got[1].VMID != 202 {
		t.Errorf("targets = %+v, want [101(explicit),202(auto)]", got)
	}
	if got[0].OS != "auto" {
		t.Errorf("explicit source must keep its own os field, got %q", got[0].OS)
	}
}

func TestDisabledSourceAndStateReset(t *testing.T) {
	a, ovr := testAgent(t, `
interval: 1s
overrideDir: "@OVR@"
sources:
  - vmid: 101
    method: ssh
    host: h
    enabled: false
ssh:
  strictHostKeys: false
`)
	var sshCalls int
	a.SSHRun = func(sshc.Options, string) (string, error) {
		sshCalls++
		return "MemTotal: 2 kB\nMemAvailable: 1 kB\n", nil
	}
	a.cycle(context.Background())
	if sshCalls != 0 {
		t.Error("disabled source must not be fetched")
	}
	_ = ovr
}

func TestSSHFailureErrorSurfacesInState(t *testing.T) {
	a, ovr := testAgent(t, `
interval: 1s
overrideDir: "@OVR@"
sources:
  - vmid: 101
    method: ssh
    host: 127.0.0.1
    os: freebsd
ssh:
  strictHostKeys: false
`)
	a.SSHRun = func(sshc.Options, string) (string, error) { return "10 20 4096", nil }
	a.cycle(context.Background())
	raw, err := os.ReadFile(filepath.Join(ovr, "101.mem"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "122880" { // (10+20)*4096
		t.Errorf("bsd override = %q", raw)
	}

	var logBuf bytes.Buffer
	a.Log = slog.New(slog.NewTextHandler(&logBuf, nil))
	a.SSHRun = func(sshc.Options, string) (string, error) { return "", errors.New("dial refused") }
	a.cycle(context.Background())
	if !strings.Contains(logBuf.String(), "fetch failed") {
		t.Errorf("expected warn log, got %q", logBuf.String())
	}
	if a.state[101].ok {
		t.Error("state must flip to failed")
	}
}

func TestWatchdogReapplies(t *testing.T) {
	a, ovr := testAgent(t, `
interval: 1s
overrideDir: "@OVR@"
`)
	var applied, restarted int
	fixture := "package PVE::QemuServer;\nsub vmstatus {\n    my $res = shift;\n    $qmpclient->queue_execute(undef, 2);\n\n    return $res;\n}\n"
	pmPath := filepath.Join(t.TempDir(), "QemuServer.pm")
	os.WriteFile(pmPath, []byte(fixture), 0o644)
	t.Setenv("PVE_QEMUSERVER_PM", pmPath)
	t.Setenv("PRM_LOCK_FILE", filepath.Join(t.TempDir(), "l"))

	a.PatchOK = func() patch.Status {
		raw, _ := os.ReadFile(pmPath)
		if strings.Contains(string(raw), patch.BeginMarker) {
			return patch.Applied
		}
		return patch.NotApplied
	}
	a.RestartPVE = func() error { restarted++; return nil }
	_ = applied
	a.lastWdAt = time.Time{}
	a.watchdog(time.Now())

	raw, _ := os.ReadFile(pmPath)
	if !strings.Contains(string(raw), patch.BeginMarker) {
		t.Error("watchdog must re-apply the patch")
	}
	if restarted != 1 {
		t.Errorf("restarts = %d", restarted)
	}
	_ = ovr
}

// Talos auto-discovery: osinfo says talos -> endpoint URL is built from
// the agent-reported IPv4 and cached.
func TestTalosAutoDiscoveryEndpoint(t *testing.T) {
	a, ovr := testAgent(t, `
interval: 1s
overrideDir: "@OVR@"
autoDiscovery:
  enabled: true
`)
	sockDir := filepath.Join(t.TempDir(), "qs")
	os.MkdirAll(sockDir, 0o755)
	os.WriteFile(filepath.Join(sockDir, "777.qga"), nil, 0o600)
	a.cfg.QGA.SocketDir = sockDir
	a.discAt = time.Time{}

	a.QGAOSInfo = func(vmid int) (*qga.OSInfo, error) {
		return &qga.OSInfo{Name: "Talos", ID: "talos"}, nil
	}
	a.QGAIPv4 = func(vmid int) (string, error) { return "10.0.0.5", nil }
	var gotURL string
	a.HTTPFetch = func(o httpc.Options) (string, error) {
		gotURL = o.URL
		return `{"total":2000,"avail":500,"used":1500}`, nil
	}

	a.cycle(context.Background())

	if gotURL != "http://10.0.0.5:1027/meminfo" {
		t.Errorf("endpoint url = %q", gotURL)
	}
	raw, err := os.ReadFile(filepath.Join(ovr, "777.mem"))
	if err != nil || strings.TrimSpace(string(raw)) != "1500" {
		t.Errorf("override = %q err=%v", raw, err)
	}
	if a.disc[777].host != "10.0.0.5" {
		t.Errorf("discovered ip not cached: %+v", a.disc[777])
	}
}

// Endpoint outage falls back to the agent's /proc/meminfo view.
func TestTalosEndpointFallback(t *testing.T) {
	a, ovr := testAgent(t, `
interval: 1s
overrideDir: "@OVR@"
sources:
  - vmid: 777
    method: qga
    os: talos
`)
	a.HTTPFetch = func(httpc.Options) (string, error) { return "", errors.New("connection refused") }
	a.QGAExec = func(vmid int, path string, args []string) (string, error) {
		return "MemTotal: 1000 kB\nMemAvailable: 400 kB\n", nil
	}

	a.cycle(context.Background())

	raw, err := os.ReadFile(filepath.Join(ovr, "777.mem"))
	if err != nil || strings.TrimSpace(string(raw)) != "614400" {
		t.Errorf("fallback override = %q err=%v", raw, err)
	}

	// Both channels dead -> loud error, no write.
	os.Remove(filepath.Join(ovr, "777.mem"))
	a.QGAExec = func(int, string, []string) (string, error) { return "", errors.New("agent down") }
	a.cycle(context.Background())
	if _, err := os.Stat(filepath.Join(ovr, "777.mem")); !os.IsNotExist(err) {
		t.Error("nothing should be written when both channels fail")
	}
}

// End-to-end through the production constructor (logger + level wiring).
func TestNewConstructor(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("logLevel: debug\noverrideDir: \""+dir+"/ovr\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := New(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if a.Log == nil || !a.Log.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("logLevel: debug must enable debug")
	}
	a.HTTPFetch = func(httpc.Options) (string, error) { return `{"used": 42}`, nil }
	if err := a.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ovr")); err != nil {
		t.Error("override dir must exist after New")
	}
}

func TestOnceReports(t *testing.T) {
	a, _ := testAgent(t, `
overrideDir: "`+t.TempDir()+`"
sources:
  - vmid: 1
    method: http
    host: x
`)
	a.HTTPFetch = func(httpc.Options) (string, error) { return `{"used": 5}`, nil }
	if err := a.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
}
