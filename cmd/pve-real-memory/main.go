// Command pve-real-memory reports real guest memory usage into Proxmox VE.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Wu-Wu/pve-real-memory/internal/agent"
	"github.com/Wu-Wu/pve-real-memory/internal/config"
	"github.com/Wu-Wu/pve-real-memory/internal/paths"
	"github.com/Wu-Wu/pve-real-memory/internal/sshc"
	"github.com/Wu-Wu/pve-real-memory/internal/svc"
)

var version = "dev"

func main() {
	args := os.Args[1:]

	// GNU-style lifecycle flags (familiar from the old installer flow).
	switch {
	case hasFlag(args, "--install"):
		ops := defaultOps()
		ops.Layout.ConfigPath = flagValue(args, "--config", paths.ConfigFile)
		if err := svc.Install(ops); err != nil {
			fatal("install", err)
		}
		return
	case hasFlag(args, "--uninstall"):
		ops := defaultOps()
		ops.Layout.ConfigPath = flagValue(args, "--config", paths.ConfigFile)
		if err := svc.Uninstall(ops, hasFlag(args, "--purge")); err != nil {
			fatal("uninstall", err)
		}
		return
	}

	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		cfgPath := fs.String("config", paths.ConfigFile, "config file")
		_ = fs.Parse(rest)
		a, err := agent.New(*cfgPath)
		if err != nil {
			fatal("config", err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		if err := a.Run(ctx); err != nil {
			fatal("run", err)
		}

	case "once":
		fs := flag.NewFlagSet("once", flag.ExitOnError)
		cfgPath := fs.String("config", paths.ConfigFile, "config file")
		_ = fs.Parse(rest)
		a, err := agent.New(*cfgPath)
		if err != nil {
			fatal("config", err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		if err := a.Once(ctx); err != nil {
			fatal("once", err)
		}

	case "status":
		fmt.Print(svc.StatusText(defaultOps()))

	case "repair":
		if err := svc.Repair(defaultOps(), hasFlag(rest, "--hook")); err != nil {
			fatal("repair", err)
		}

	case "trust-host":
		fs := flag.NewFlagSet("trust-host", flag.ExitOnError)
		cfgPath := fs.String("config", paths.ConfigFile, "config file")
		user := fs.String("user", "", "ssh user (default: config ssh.user)")
		port := fs.Int("port", 0, "ssh port (default: config ssh.port)")
		key := fs.String("key", "", "private key (default: config ssh.privateKey)")
		_ = fs.Parse(rest)
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: pve-real-memory trust-host [--user u] [--port p] [--key k] <host>")
			os.Exit(2)
		}
		fp, err := trustHost(*cfgPath, *user, *port, *key, fs.Arg(0))
		if err != nil {
			fatal("trust-host", err)
		}
		fmt.Printf("trusted %s (%s)\n", fs.Arg(0), fp)

	case "forget-host":
		fs := flag.NewFlagSet("forget-host", flag.ExitOnError)
		cfgPath := fs.String("config", paths.ConfigFile, "config file")
		port := fs.Int("port", 0, "ssh port")
		_ = fs.Parse(rest)
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: pve-real-memory forget-host [--port p] <host>")
			os.Exit(2)
		}
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fatal("config", err)
		}
		p := *port
		if p == 0 {
			p = cfg.SSH.Port
		}
		n, err := sshc.ForgetHost(cfg.SSH.KnownHosts, fs.Arg(0), p)
		if err != nil {
			fatal("forget-host", err)
		}
		fmt.Printf("removed %d known_hosts entries for %s\n", n, fs.Arg(0))

	case "doctor":
		fs := flag.NewFlagSet("doctor", flag.ExitOnError)
		cfgPath := fs.String("config", paths.ConfigFile, "config file")
		_ = fs.Parse(rest)
		if err := doctor(*cfgPath); err != nil {
			fatal("doctor", err)
		}

	case "version", "--version":
		fmt.Println(version)

	default:
		usage()
		os.Exit(2)
	}
}

// doctor validates config and reports transport reachability without
// touching PVE — safe for support tickets.
func doctor(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	fmt.Printf("config %s: ok (%d sources, auto=%v, interval=%s)\n",
		cfgPath, len(cfg.Sources), cfg.Auto.Enabled, cfg.Interval.String())

	// ssh key sanity
	if _, err := os.Stat(cfg.SSH.PrivateKey); err == nil {
		fmt.Printf("ssh key %s: present\n", cfg.SSH.PrivateKey)
	} else {
		fmt.Printf("ssh key %s: MISSING (ssh sources will fail; run --install to generate)\n", cfg.SSH.PrivateKey)
	}
	fmt.Printf("known_hosts %s: ", cfg.SSH.KnownHosts)
	if raw, err := os.ReadFile(cfg.SSH.KnownHosts); err != nil {
		fmt.Println("missing — strict mode will refuse all hosts until `trust-host`")
	} else {
		fmt.Printf("%d lines\n", countLines(raw))
	}

	// override dir + hook
	if _, err := os.Stat(cfg.OverrideDir); err != nil {
		fmt.Printf("override dir %s: missing (created by systemd or at first write)\n", cfg.OverrideDir)
	} else {
		ents, _ := os.ReadDir(cfg.OverrideDir)
		fmt.Printf("override dir %s: %d entries\n", cfg.OverrideDir, len(ents))
	}

	fmt.Print("\n", svc.StatusText(defaultOps()))
	return nil
}

func countLines(raw []byte) int {
	n := 1
	for _, b := range raw {
		if b == '\n' {
			n++
		}
	}
	return n
}

func trustHost(cfgPath, user string, port int, key, host string) (string, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return "", err
	}
	u := cfg.SSH.User
	if user != "" {
		u = user
	}
	p := cfg.SSH.Port
	if port != 0 {
		p = port
	}
	k := cfg.SSH.PrivateKey
	if key != "" {
		k = key
	}
	return sshc.TrustHost(sshc.Options{
		User: u, Port: p, Host: host, PrivateKey: k,
		KnownHosts: cfg.SSH.KnownHosts, Timeout: cfg.SSH.Timeout.Duration(),
	})
}

func defaultOps() *svc.Ops {
	return &svc.Ops{Layout: svc.DefaultLayout(), Out: os.Stdout}
}

func hasFlag(args []string, f string) bool {
	for _, a := range args {
		if a == f {
			return true
		}
	}
	return false
}

// flagValue reads "--flag value" or "--flag=value" without a FlagSet
// (the lifecycle flags must coexist with positional verbs).
func flagValue(args []string, f, def string) string {
	for i, a := range args {
		if a == f && i+1 < len(args) {
			return args[i+1]
		}
		if len(a) > len(f)+1 && a[:len(f)+1] == f+"=" {
			return a[len(f)+1:]
		}
	}
	return def
}

func fatal(what string, err error) {
	slog.New(slog.NewTextHandler(os.Stderr, nil)).Error(what+" failed", "err", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `pve-real-memory `+version+` — real guest memory reporting for Proxmox VE

usage:
  pve-real-memory --install [--config f]     install layer and start daemon
  pve-real-memory --uninstall [--purge]      reverse the install
  pve-real-memory run [--config f]           run the monitoring daemon
  pve-real-memory once [--config f]          one fetch pass, print results
  pve-real-memory status                     patch/service/override status
  pve-real-memory doctor [--config f]        validate config, keys, hooks
  pve-real-memory repair [--hook]            re-apply vmstatus patch
  pve-real-memory trust-host [--user u] <h>  pin a guest SSH host key
  pve-real-memory forget-host <h>            drop a guest SSH host key
  pve-real-memory version

config: `+paths.ConfigFile+` (live reload; see that file for the schema)
`)
}
