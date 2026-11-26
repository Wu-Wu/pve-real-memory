// Package agent is the monitoring loop: it resolves the VM target list
// (explicit config + auto-discovery), fetches real memory through the
// configured transport, maintains override files, prunes stale ones and
// watches that the vmstatus hook is still alive after PVE upgrades.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/Wu-Wu/pve-real-memory/internal/config"
	"github.com/Wu-Wu/pve-real-memory/internal/httpc"
	"github.com/Wu-Wu/pve-real-memory/internal/memsrc"
	"github.com/Wu-Wu/pve-real-memory/internal/override"
	"github.com/Wu-Wu/pve-real-memory/internal/patch"
	"github.com/Wu-Wu/pve-real-memory/internal/paths"
	"github.com/Wu-Wu/pve-real-memory/internal/qga"
	"github.com/Wu-Wu/pve-real-memory/internal/sshc"
	"github.com/Wu-Wu/pve-real-memory/internal/system"
)

// Agent is the monitoring daemon.
type Agent struct {
	CfgPath string
	Log     *slog.Logger
	lv      *slog.LevelVar

	// Transports are overridable for tests.
	QGAExec    func(vmid int, path string, args []string) (string, error)
	QGAOSInfo  func(vmid int) (*qga.OSInfo, error)
	QGAIPv4    func(vmid int) (string, error)
	SSHRun     func(o sshc.Options, cmd string) (string, error)
	HTTPFetch  func(o httpc.Options) (string, error)
	RestartPVE func() error
	PatchOK    func() patch.Status

	cfg      *config.Config
	cfgMod   time.Time
	store    *override.Store
	qgaRun   *qga.Runner
	state    map[int]*vmState
	disc     map[int]discEntry
	discAt   time.Time
	lastWdAt time.Time
}

type vmState struct {
	ok       bool
	value    int64
	lastErr  string
	lastInfo time.Time
}

type discEntry struct {
	os    string
	host  string // discovered IPv4 (Talos endpoint resolution)
	added time.Time
}

// New loads the config and wires production transports. The logger is
// built from config logLevel and re-leveled on reload.
func New(cfgPath string) (*Agent, error) {
	a := &Agent{
		CfgPath: cfgPath,
		lv:      &slog.LevelVar{},
		state:   map[int]*vmState{},
		disc:    map[int]discEntry{},
	}
	var err error
	a.cfg, a.cfgMod, err = a.loadConfig()
	if err != nil {
		return nil, err
	}
	a.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: a.lv}))
	a.applyLevel()
	a.applyConfig()
	a.RestartPVE = func() error { return system.RestartUnits(system.PVERestartUnits...) }
	a.PatchOK = func() patch.Status {
		return (&patch.Engine{Path: pmPath(), OverrideDir: a.cfg.OverrideDir, TTLSeconds: config.PatchTTLSeconds}).Status()
	}
	return a, nil
}

func (a *Agent) applyLevel() {
	if a.lv == nil { // hand-constructed test agents
		return
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(a.cfg.LogLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	a.lv.Set(lvl)
}

func pmPath() string {
	if v := os.Getenv("PVE_QEMUSERVER_PM"); v != "" {
		return v
	}
	return paths.QemuServerPM
}

func (a *Agent) loadConfig() (*config.Config, time.Time, error) {
	fi, err := os.Stat(a.CfgPath)
	if err != nil {
		return nil, time.Time{}, err
	}
	cfg, err := config.Load(a.CfgPath)
	if err != nil {
		return nil, time.Time{}, err
	}
	return cfg, fi.ModTime(), nil
}

func (a *Agent) applyConfig() {
	a.store = &override.Store{Dir: a.cfg.OverrideDir}
	_ = os.MkdirAll(a.cfg.OverrideDir, 0o700)
	a.qgaRun = qga.NewRunner(a.cfg.QGA.SocketDir, a.cfg.QGA.ExecTimeout.Duration())
	// (Re)bind QGA to the current runner. Tests inject transports after
	// New() and do not reload config.
	a.QGAExec = a.qgaRun.Exec
	a.QGAOSInfo = a.qgaRun.GetOSInfo
	a.QGAIPv4 = a.qgaRun.GetIPv4
	a.SSHRun = sshc.Run
	a.HTTPFetch = httpc.Fetch
}

// reloadIfChanged re-reads config when the file changed; logs and keeps
// per-VM state across reloads.
func (a *Agent) reloadIfChanged() {
	fi, err := os.Stat(a.CfgPath)
	if err != nil {
		a.Log.Warn("config stat failed, keeping current config", "err", err)
		return
	}
	if fi.ModTime().Equal(a.cfgMod) {
		return
	}
	cfg, err := config.Load(a.CfgPath)
	if err != nil {
		a.Log.Error("config reload failed, keeping current config", "err", err)
		a.cfgMod = fi.ModTime() // don't spam every cycle
		return
	}
	a.cfg = cfg
	a.cfgMod = fi.ModTime()
	a.applyLevel()
	a.applyConfig()
	a.Log.Info("config reloaded", "sources", len(cfg.Sources), "auto", cfg.Auto.Enabled)
}

// Targets = enabled explicit sources + cached discoveries not explicitly
// configured.
func (a *Agent) Targets() []config.Source {
	out := make([]config.Source, 0, len(a.cfg.Sources))
	explicit := map[int]bool{}
	for _, s := range a.cfg.Sources {
		if s.EnabledOrTrue() {
			out = append(out, s)
		}
		explicit[int(s.VMID)] = true
	}
	if a.cfg.Auto.Enabled {
		for vmid, e := range a.disc {
			if explicit[vmid] || excluded(a.cfg.Auto.Exclude, vmid) {
				continue
			}
			out = append(out, config.Source{
				VMID:   config.VMID(vmid),
				Method: config.MethodQGA,
				OS:     e.os,
			})
		}
	}
	// Deterministic order for logs and tests.
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out
}

func excluded(list []config.VMID, vmid int) bool {
	for _, v := range list {
		if int(v) == vmid {
			return true
		}
	}
	return false
}

// discover rescans QGA sockets and refreshes the OS cache.
func (a *Agent) discover(now time.Time) {
	if !a.cfg.Auto.Enabled {
		return
	}
	if !a.discAt.IsZero() && now.Sub(a.discAt) < a.cfg.Auto.ScanInterval.Duration() {
		return
	}
	a.discAt = now

	ids, err := qga.ListVMIDs(a.cfg.QGA.SocketDir)
	if err != nil {
		a.Log.Warn("auto-discovery scan failed", "err", err)
		return
	}
	explicit := map[int]bool{}
	for _, s := range a.cfg.Sources {
		explicit[int(s.VMID)] = true
	}

	found := map[int]bool{}
	for _, vmid := range ids {
		if explicit[vmid] || excluded(a.cfg.Auto.Exclude, vmid) {
			continue
		}
		found[vmid] = true
		if _, cached := a.disc[vmid]; cached {
			continue
		}
		info, err := a.QGAOSInfo(vmid)
		if err != nil {
			a.Log.Debug("auto-discovery: osinfo failed", "vmid", vmid, "err", err)
			continue // not cached, retried next scan
		}
		osType := info.Detect()
		if osType == "" {
			a.Log.Debug("auto-discovery: os not detected", "vmid", vmid)
			continue
		}
		a.disc[vmid] = discEntry{os: osType, added: now}
		a.Log.Info("auto-discovery: watching vm", "vmid", vmid, "os", osType)
	}
	for vmid, e := range a.disc {
		if !found[vmid] {
			delete(a.disc, vmid)
			a.Log.Info("auto-discovery: no longer watching vm", "vmid", vmid)
		}
		_ = e
	}
}

// fetch resolves one source through its transport.
func (a *Agent) fetch(s config.Source) (int64, error) {
	switch s.Method {
	case config.MethodQGA:
		osType := s.OS
		if osType == config.OSAuto {
			e, cached := a.disc[int(s.VMID)]
			if !cached {
				info, err := a.QGAOSInfo(int(s.VMID))
				if err != nil {
					return 0, fmt.Errorf("qga os detection: %w", err)
				}
				osType = info.Detect()
				if osType == "" {
					return 0, errors.New("qga agent does not report an OS identity; set os explicitly")
				}
				a.disc[int(s.VMID)] = discEntry{os: osType, added: time.Now()}
			} else {
				osType = e.os
			}
		}
		switch osType {
		case "talos":
			return a.fetchTalos(s, int(s.VMID))
		case "linux":
			out, err := a.QGAExec(int(s.VMID), memsrc.LinuxCommand.Path, memsrc.LinuxCommand.Args)
			if err != nil {
				return 0, err
			}
			return memsrc.ParseLinuxMeminfo(out)
		case "freebsd":
			out, err := a.QGAExec(int(s.VMID), memsrc.FreeBSDCommand.Path, memsrc.FreeBSDCommand.Args)
			if err != nil {
				return 0, err
			}
			return memsrc.ParseFreeBSDSysctl(out)
		case "windows":
			out, err := a.QGAExec(int(s.VMID), memsrc.WindowsCommand.Path, memsrc.WindowsCommand.Args)
			if err != nil {
				return 0, err
			}
			return memsrc.ParseWindowsCIM(out)
		}
		return 0, fmt.Errorf("qga: unknown os %q", osType)

	case config.MethodSSH:
		cmd, err := memsrc.SSHCommand(s.OS)
		if err != nil {
			return 0, err
		}
		out, err := a.SSHRun(sshc.Options{
			User: s.User, Port: s.Port, PrivateKey: s.PrivateKey,
			Host: s.Host, Strict: a.cfg.StrictHostKeys(),
			KnownHosts: a.cfg.SSH.KnownHosts, Timeout: a.cfg.SSH.Timeout.Duration(),
		}, cmd)
		if err != nil {
			return 0, err
		}
		return memsrc.ParseForOS(s.OS, out)

	case config.MethodHTTP:
		out, err := a.HTTPFetch(httpc.Options{
			URL: config.SourceURL(&s), Token: s.Token, Headers: s.Headers,
			Insecure: s.Insecure != nil && *s.Insecure, Timeout: a.cfg.HTTP.Timeout.Duration(),
		})
		if err != nil {
			return 0, err
		}
		return memsrc.ParseMeminfoJSON(out)
	}
	return 0, fmt.Errorf("unknown method %q", s.Method)
}

// fetchTalos collects from a Talos guest: the meminfo-service endpoint
// (the Talos system extension) is authoritative; the agent's view of
// /proc/meminfo is the fallback for endpoint outages. The endpoint host
// comes from the source, or — for auto-discovered guests — from the
// agent's reported IPv4, cached with the discovery entry.
func (a *Agent) fetchTalos(s config.Source, vmid int) (int64, error) {
	url := s.URL
	if url == "" {
		host := s.Host
		if host == "" {
			e := a.disc[vmid]
			host = e.host
			if host == "" {
				ip, err := a.QGAIPv4(vmid)
				if err != nil {
					a.Log.Debug("talos: no endpoint host (agent gave no IPv4), falling back to /proc/meminfo", "vmid", vmid, "err", err)
				} else {
					host = ip
					e.host = ip
					if _, known := a.disc[vmid]; known || a.cfg.Auto.Enabled {
						a.disc[vmid] = e
					}
				}
			}
		}
		if host != "" {
			url = fmt.Sprintf("http://%s:%d%s", host, config.DefaultTalosPort, config.DefaultTalosPath)
		}
	}

	var endpointErr error
	if url != "" {
		out, err := a.HTTPFetch(httpc.Options{
			URL: url, Token: s.Token, Headers: s.Headers,
			Insecure: s.Insecure != nil && *s.Insecure, Timeout: a.cfg.HTTP.Timeout.Duration(),
		})
		if err == nil {
			v, perr := memsrc.ParseMeminfoJSON(out)
			if perr == nil {
				return v, nil
			}
			endpointErr = perr
		} else {
			endpointErr = err
		}
		a.Log.Debug("talos: meminfo endpoint failed, falling back to /proc/meminfo", "vmid", vmid, "url", url, "err", endpointErr)
	}

	out, err := a.QGAExec(vmid, memsrc.LinuxCommand.Path, memsrc.LinuxCommand.Args)
	if err != nil {
		if endpointErr != nil {
			return 0, fmt.Errorf("meminfo endpoint: %v; qga fallback: %w", endpointErr, err)
		}
		return 0, fmt.Errorf("qga /proc/meminfo fallback: %w", err)
	}
	v, perr := memsrc.ParseLinuxMeminfo(out)
	if perr != nil {
		if endpointErr != nil {
			return 0, fmt.Errorf("meminfo endpoint: %v; qga fallback parse: %w", endpointErr, perr)
		}
		return 0, perr
	}
	return v, nil
}

// cycle runs one full pass.
func (a *Agent) cycle(ctx context.Context) {
	now := time.Now()
	a.discover(now)

	targets := a.Targets()
	type result struct {
		src   config.Source
		value int64
		err   error
	}
	results := make([]result, len(targets))

	sem := make(chan struct{}, a.cfg.MaxParallel)
	var wg sync.WaitGroup
	for i, src := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int, src config.Source) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v, err := a.fetch(src)
			results[i] = result{src, v, err}
		}(i, src)
	}
	wg.Wait()

	active := map[int]bool{}
	for _, r := range results {
		vmid := int(r.src.VMID)
		active[vmid] = true
		st, had := a.state[vmid]
		if !had {
			st = &vmState{}
			a.state[vmid] = st
		}
		if r.err != nil {
			if st.ok || st.lastErr != r.err.Error() {
				a.Log.Warn("fetch failed", "vmid", vmid, "method", r.src.Method, "os", r.src.OS, "err", r.err)
			}
			st.ok = false
			st.lastErr = r.err.Error()
			continue // existing override file simply ages out
		}
		if err := a.store.Write(vmid, r.value); err != nil {
			a.Log.Error("override write failed", "vmid", vmid, "err", err)
			continue
		}
		switch {
		case !st.ok:
			a.Log.Info("fetch ok", "vmid", vmid, "method", r.src.Method, "os", r.src.OS, "usedMB", r.value>>20)
		case r.value != st.value:
			a.Log.Debug("updated", "vmid", vmid, "usedMB", r.value>>20)
		case now.Sub(st.lastInfo) >= time.Minute:
			a.Log.Info("still ok", "vmid", vmid, "usedMB", r.value>>20)
			st.lastInfo = now
		}
		st.ok = true
		st.value = r.value
		st.lastErr = ""
	}

	// Drop state and overrides for VMs no longer targeted.
	for vmid := range a.state {
		if !active[vmid] {
			delete(a.state, vmid)
		}
	}
	removed, err := a.store.Prune(active)
	if err != nil {
		a.Log.Warn("override prune failed", "err", err)
	}
	for _, vmid := range removed {
		a.Log.Info("override removed (vm no longer monitored)", "vmid", vmid)
	}
}

// watchdog re-applies the vmstatus hook if a PVE upgrade wiped it.
func (a *Agent) watchdog(now time.Time) {
	if !a.cfg.AutoRepairEnabled() {
		return
	}
	if !a.lastWdAt.IsZero() && now.Sub(a.lastWdAt) < time.Minute {
		return
	}
	a.lastWdAt = now

	switch a.PatchOK() {
	case patch.Applied:
		return
	case patch.FileMissing:
		a.Log.Warn("QemuServer.pm not found; not a PVE host or wrong path")
		return
	}

	a.Log.Warn("vmstatus patch missing (Proxmox update?); re-applying")
	eng := &patch.Engine{
		Path:        pmPath(),
		OverrideDir: a.cfg.OverrideDir,
		TTLSeconds:  config.PatchTTLSeconds,
		Verifier:    system.PerlCompile,
	}
	lock, err := system.AcquireLock(lockPath())
	if err != nil {
		a.Log.Warn("patch repair skipped (lock held)", "err", err)
		return
	}
	defer lock.Release()
	applied, fallback, err := eng.Apply()
	if err != nil {
		a.Log.Error("patch re-apply failed", "err", err)
		return
	}
	if applied {
		if fallback {
			a.Log.Warn("patch applied via fallback anchor; please review the diff")
		}
		if err := a.RestartPVE(); err != nil {
			a.Log.Error("patch applied but pvestatd/pvedaemon restart failed", "err", err)
			return
		}
		a.Log.Info("patch re-applied and services restarted")
	}
}

func lockPath() string {
	if v := os.Getenv("PRM_LOCK_FILE"); v != "" {
		return v
	}
	return paths.LockFile
}

// Run is the daemon loop (until ctx cancel), reloading config on change
// and SIGHUP.
func (a *Agent) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	a.Log.Info("starting", "config", a.CfgPath, "interval", a.cfg.Interval.String(),
		"sources", len(a.cfg.Sources), "auto", a.cfg.Auto.Enabled)

	if a.PatchOK() != patch.Applied {
		a.Log.Warn("vmstatus patch is NOT applied; overrides will have no visible effect (run --install or check autoRepair)")
	}

	a.runOneCycle(ctx)
	for {
		timer := time.NewTimer(a.cfg.Interval.Duration())
		select {
		case <-ctx.Done():
			timer.Stop()
			a.Log.Info("shutting down")
			return nil
		case <-timer.C:
			a.runOneCycle(ctx)
		}
	}
}

func (a *Agent) runOneCycle(ctx context.Context) {
	a.reloadIfChanged()
	a.cycle(ctx)
	a.watchdog(time.Now())
}

// Once executes exactly one pass and returns the first error encountered
// (used by `pve-real-memory once`).
func (a *Agent) Once(ctx context.Context) error {
	a.reloadIfChanged()
	a.discover(time.Now())

	targets := a.Targets()
	var firstErr error
	var failures int
	for _, src := range targets {
		v, err := a.fetch(src)
		vmid := int(src.VMID)
		if err != nil {
			failures++
			fmt.Printf("vmid %-6d %-6s %-8s ERROR  %v\n", vmid, src.Method, src.OS, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := a.store.Write(vmid, v); err != nil {
			failures++
			fmt.Printf("vmid %-6d %-6s %-8s ERROR  write override: %v\n", vmid, src.Method, src.OS, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fmt.Printf("vmid %-6d %-6s %-8s ok     used=%d (%.1f MiB) -> %s\n", vmid, src.Method, src.OS, v, float64(v)/1048576, a.store.Path(vmid))
	}
	if len(targets) == 0 {
		fmt.Println("no sources configured (add sources[] or enable autoDiscovery)")
	}
	if failures > 0 {
		return fmt.Errorf("%d/%d sources failed", failures, len(targets))
	}
	return nil
}
