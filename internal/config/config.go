// Package config defines the YAML configuration schema, its defaults and
// validation. The file is re-read at runtime, so the schema stays forgiving
// about types (e.g. "101" is accepted for an int vmid) but strict about
// semantics: every field must be known and every source fully specified.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Wu-Wu/pve-real-memory/internal/paths"
)

// Duration is a time.Duration that (un)marshals from strings like "5s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if v < 0 {
		return fmt.Errorf("negative duration %q", s)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// VMID accepts both YAML integers and quoted strings ("101"), normalizing
// the value to an int. The legacy config's string-vs-int mismatch must not
// silently create duplicate VM entries.
type VMID int

func (v *VMID) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || fmt.Sprint(n) != s {
		return fmt.Errorf("invalid vmid %q: must be a plain integer", s)
	}
	if n <= 0 {
		return fmt.Errorf("invalid vmid %d", n)
	}
	*v = VMID(n)
	return nil
}

// Methods supported by Source.Method.
const (
	MethodQGA  = "qga"
	MethodSSH  = "ssh"
	MethodHTTP = "http"
)

// Operating systems supported by Source.OS ("auto" only works with QGA).
const (
	OSAuto    = "auto"
	OSLinux   = "linux"
	OSFreeBSD = "freebsd"
	OSWindows = "windows"
)

// SSHDefaults are the global SSH transport settings; individual sources may
// override user/port/key.
type SSHDefaults struct {
	User           string   `yaml:"user"`
	Port           int      `yaml:"port"`
	PrivateKey     string   `yaml:"privateKey"`
	KnownHosts     string   `yaml:"knownHosts"`
	StrictHostKeys *bool    `yaml:"strictHostKeys"`
	Timeout        Duration `yaml:"timeout"`
}

// QGADefaults are global QEMU guest agent settings.
type QGADefaults struct {
	SocketDir   string   `yaml:"socketDir"`
	ExecTimeout Duration `yaml:"execTimeout"`
}

// HTTPDefaults are global HTTP transport settings for the Talos meminfo
// endpoint (and anything else speaking the same JSON protocol).
type HTTPDefaults struct {
	Timeout  Duration `yaml:"timeout"`
	Insecure *bool    `yaml:"insecure"`
}

// Auto controls discovery of QGA-enabled VMs on this node.
type Auto struct {
	Enabled      bool     `yaml:"enabled"`
	ScanInterval Duration `yaml:"scanInterval"`
	Exclude      []VMID   `yaml:"exclude"`
}

// Source is one monitored VM.
type Source struct {
	VMID    VMID   `yaml:"vmid"`
	Method  string `yaml:"method"`
	OS      string `yaml:"os"`
	Enabled *bool  `yaml:"enabled"`

	// SSH transport fields.
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	User       string `yaml:"user"`
	PrivateKey string `yaml:"privateKey"`

	// HTTP transport fields. URL defaults to http://<host>:1027/meminfo,
	// the meminfo-service Talos extension endpoint.
	URL      string            `yaml:"url"`
	Token    string            `yaml:"token"`
	Headers  map[string]string `yaml:"headers"`
	Insecure *bool             `yaml:"insecure"`
}

// Config is the top-level schema.
type Config struct {
	Interval    Duration     `yaml:"interval"`
	MaxParallel int          `yaml:"maxParallel"`
	OverrideDir string       `yaml:"overrideDir"`
	LogLevel    string       `yaml:"logLevel"`
	AutoRepair  *bool        `yaml:"autoRepair"`
	PatchTTLSec int          `yaml:"-"` // fixed by the embedded Perl patch
	SSH         SSHDefaults  `yaml:"ssh"`
	QGA         QGADefaults  `yaml:"qga"`
	HTTP        HTTPDefaults `yaml:"http"`
	Auto        Auto         `yaml:"autoDiscovery"`
	Sources     []Source     `yaml:"sources"`
}

// Defaults and hard limits.
const (
	DefaultInterval     = 5 * time.Second
	MinInterval         = time.Second
	DefaultMaxParallel  = 8
	MaxParallelLimit    = 64
	DefaultTimeout      = 10 * time.Second
	DefaultScanInterval = 60 * time.Second
	DefaultTalosPort    = 1027
	DefaultTalosPath    = "/meminfo"
	PatchTTLSeconds     = 120 // embedded in the Perl patch; overrides older than this are ignored
)

// Load reads, decodes, defaults and validates the config at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes, defaults and validates config bytes.
func Parse(raw []byte) (*Config, error) {
	cfg := &Config{}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config parse: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func boolPtr(v bool) *bool { return &v }

func (c *Config) applyDefaults() {
	if c.Interval == 0 {
		c.Interval = Duration(DefaultInterval)
	}
	if c.MaxParallel == 0 {
		c.MaxParallel = DefaultMaxParallel
	}
	if c.OverrideDir == "" {
		c.OverrideDir = paths.OverrideDir
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.AutoRepair == nil {
		c.AutoRepair = boolPtr(true)
	}
	if c.SSH.User == "" {
		c.SSH.User = "root"
	}
	if c.SSH.Port == 0 {
		c.SSH.Port = 22
	}
	if c.SSH.PrivateKey == "" {
		c.SSH.PrivateKey = paths.SSHKeyDefault
	}
	if c.SSH.KnownHosts == "" {
		c.SSH.KnownHosts = paths.KnownHosts
	}
	if c.SSH.StrictHostKeys == nil {
		c.SSH.StrictHostKeys = boolPtr(true)
	}
	if c.SSH.Timeout == 0 {
		c.SSH.Timeout = Duration(DefaultTimeout)
	}
	if c.QGA.SocketDir == "" {
		c.QGA.SocketDir = paths.QGAServiceDir
	}
	if c.QGA.ExecTimeout == 0 {
		c.QGA.ExecTimeout = Duration(DefaultTimeout)
	}
	if c.HTTP.Timeout == 0 {
		c.HTTP.Timeout = Duration(DefaultTimeout)
	}
	if c.HTTP.Insecure == nil {
		c.HTTP.Insecure = boolPtr(false)
	}
	if c.Auto.ScanInterval == 0 {
		c.Auto.ScanInterval = Duration(DefaultScanInterval)
	}
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.Method == "" {
			s.Method = MethodQGA
		}
		if s.OS == "" {
			s.OS = OSAuto
		}
		if s.Enabled == nil {
			s.Enabled = boolPtr(true)
		}
		if s.Port == 0 {
			s.Port = c.SSH.Port
		}
		if s.User == "" {
			s.User = c.SSH.User
		}
		if s.PrivateKey == "" {
			s.PrivateKey = c.SSH.PrivateKey
		}
	}
}

// Validate enforces semantics the type system cannot.
func (c *Config) Validate() error {
	var errs []string

	if c.Interval.Duration() < MinInterval {
		errs = append(errs, fmt.Sprintf("interval must be at least %s", MinInterval))
	}
	if c.MaxParallel < 1 || c.MaxParallel > MaxParallelLimit {
		errs = append(errs, fmt.Sprintf("maxParallel must be in [1,%d]", MaxParallelLimit))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, "logLevel must be one of debug|info|warn|error")
	}

	seen := map[VMID]string{}
	for i := range c.Sources {
		s := &c.Sources[i]
		tag := fmt.Sprintf("sources[%d] (vmid %d)", i, s.VMID)

		if prev, dup := seen[s.VMID]; dup {
			errs = append(errs, fmt.Sprintf("%s: vmid %d already configured as %s", tag, s.VMID, prev))
		}
		seen[s.VMID] = s.Method

		switch s.Method {
		case MethodQGA, MethodSSH, MethodHTTP:
		default:
			errs = append(errs, fmt.Sprintf("%s: unknown method %q (want qga|ssh|http)", tag, s.Method))
			continue
		}

		switch s.OS {
		case OSAuto, OSLinux, OSFreeBSD, OSWindows:
		default:
			errs = append(errs, fmt.Sprintf("%s: unknown os %q (want auto|linux|freebsd|windows)", tag, s.OS))
		}

		switch s.Method {
		case MethodSSH:
			if s.Host == "" {
				errs = append(errs, tag+": ssh method requires host")
			}
			if s.OS == OSAuto {
				// No probe channel exists for plain SSH; default it.
				s.OS = OSLinux
			}
			if s.OS == OSWindows {
				errs = append(errs, tag+": windows guests are supported via qga only")
			}
		case MethodHTTP:
			if s.URL == "" && s.Host == "" {
				errs = append(errs, tag+": http method requires url or host")
			}
		}
	}

	if len(errs) > 0 {
		return errors.New("config invalid:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

// AutoRepairEnabled reports whether the runtime patch watchdog is on.
func (c *Config) AutoRepairEnabled() bool {
	return c.AutoRepair != nil && *c.AutoRepair
}

// StrictHostKeys reports SSH host key policy.
func (c *Config) StrictHostKeys() bool {
	return c.SSH.StrictHostKeys != nil && *c.SSH.StrictHostKeys
}

// SourceURL resolves the HTTP endpoint for a source: explicit url wins,
// otherwise the meminfo-service default is built from host.
func SourceURL(s *Source) string {
	if s.URL != "" {
		return s.URL
	}
	return fmt.Sprintf("http://%s:%d%s", s.Host, DefaultTalosPort, DefaultTalosPath)
}

// Enabled reports whether a source is active.
func (s *Source) EnabledOrTrue() bool {
	return s.Enabled == nil || *s.Enabled
}
