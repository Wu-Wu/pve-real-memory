package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte("autoDiscovery:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Interval.Duration() != DefaultInterval {
		t.Errorf("interval default = %v, want %v", cfg.Interval, DefaultInterval)
	}
	if cfg.MaxParallel != DefaultMaxParallel {
		t.Errorf("maxParallel default = %d", cfg.MaxParallel)
	}
	if cfg.SSH.User != "root" || cfg.SSH.Port != 22 {
		t.Errorf("ssh defaults wrong: %+v", cfg.SSH)
	}
	if !cfg.StrictHostKeys() {
		t.Error("strictHostKeys must default to true")
	}
	if !cfg.AutoRepairEnabled() {
		t.Error("autoRepair must default to true")
	}
	if cfg.OverrideDir == "" {
		t.Error("overrideDir must default")
	}
}

func TestParseQuotedVMIDNormalizes(t *testing.T) {
	// The legacy config's classic footgun: "vmid": "101" as a string.
	cfg, err := Parse([]byte(`
sources:
  - vmid: "101"
    method: http
    host: 10.0.0.5
`))
	if err != nil {
		t.Fatalf("quoted vmid must be accepted: %v", err)
	}
	if cfg.Sources[0].VMID != 101 {
		t.Errorf("vmid = %d, want 101", cfg.Sources[0].VMID)
	}
}

func TestParseRejectsBadVMID(t *testing.T) {
	for _, s := range []string{`"x"`, `" 10"`, `"10 "`, `"0"`, `"-3"`} {
		if _, err := Parse([]byte("sources:\n  - vmid: " + s + "\n")); err == nil {
			t.Errorf("vmid %s must be rejected", s)
		}
	}
}

func TestSourceDefaultsAndURL(t *testing.T) {
	cfg, err := Parse([]byte(`
sources:
  - vmid: 303
    method: http
    host: 192.168.1.30
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := cfg.Sources[0]
	if s.OS != OSAuto {
		t.Errorf("os default = %q", s.OS)
	}
	if !s.EnabledOrTrue() {
		t.Error("enabled must default to true")
	}
	if got := SourceURL(&s); got != "http://192.168.1.30:1027/meminfo" {
		t.Errorf("SourceURL = %q", got)
	}
}

func TestValidateErrors(t *testing.T) {
	tests := map[string]string{
		"duplicate vmid": `
sources:
  - vmid: 101
    method: qga
  - vmid: 101
    method: http
    host: h
`,
		"ssh without host": `
sources:
  - vmid: 101
    method: ssh
`,
		"windows over ssh": `
sources:
  - vmid: 101
    method: ssh
    host: h
    os: windows
`,
		"unknown method": `
sources:
  - vmid: 101
    method: carrier-pigeon
`,
		"unknown field": `
intervalle: 5s
`,
		"bad duration": `
interval: 5secs
`,
		"http without target": `
sources:
  - vmid: 101
    method: http
`,
	}
	for name, body := range tests {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestSSHAutoDefaultsToLinux(t *testing.T) {
	cfg, err := Parse([]byte(`
sources:
  - vmid: 101
    method: ssh
    host: 10.0.0.1
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Sources[0].OS != OSLinux {
		t.Errorf("ssh+auto os = %q, want linux", cfg.Sources[0].OS)
	}
}

func TestDisabledSourceStaysDisabled(t *testing.T) {
	cfg, err := Parse([]byte(`
sources:
  - vmid: 101
    method: qga
    enabled: false
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Sources[0].EnabledOrTrue() {
		t.Error("enabled:false must stick")
	}
}

func TestDurationUnitsAreReal(t *testing.T) {
	cfg, err := Parse([]byte("interval: 1m30s\nmaxParallel: 3\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Interval.Duration() != 90*time.Second {
		t.Errorf("interval = %v", cfg.Interval)
	}
}

func TestIntervalFloor(t *testing.T) {
	if _, err := Parse([]byte("interval: 100ms\n")); err == nil {
		t.Error("sub-second interval must be rejected")
	} else if !strings.Contains(err.Error(), "at least") {
		t.Errorf("error text = %v", err)
	}
}
