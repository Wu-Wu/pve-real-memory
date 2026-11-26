// Package sshc implements the SSH transport: key parsing (OpenSSH, PEM,
// PKCS#8; encrypted keys are reported, not silently skipped), host key
// verification against a known_hosts file, and a hard overall timeout —
// the two things the Python version got wrong (hardcoded root, no host
// verification, hangable connections).
package sshc

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Options for one fetch.
type Options struct {
	User       string
	Port       int
	PrivateKey string // path; may be "" to use AgentAuth if extended later
	Passphrase string // optional
	Host       string // host or ip
	Strict     bool   // verify host keys
	KnownHosts string // known_hosts path when Strict
	Timeout    time.Duration
}

// Run executes cmd over a fresh connection and returns stdout.
func Run(o Options, cmd string) (string, error) {
	if o.Host == "" {
		return "", errors.New("ssh: host not configured")
	}
	if o.User == "" {
		return "", errors.New("ssh: user not configured")
	}
	if o.Port == 0 {
		o.Port = 22
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}

	cfg := &ssh.ClientConfig{
		User:            o.User,
		Timeout:         o.Timeout, // dial timeout
		HostKeyCallback: o.hostKeyCallback(),
	}
	if o.PrivateKey != "" {
		auth, err := privateKeyAuth(o.PrivateKey, o.Passphrase)
		if err != nil {
			return "", err
		}
		cfg.Auth = append(cfg.Auth, auth)
	}

	addr := net.JoinHostPort(o.Host, fmt.Sprint(o.Port))
	conn, err := net.DialTimeout("tcp", addr, o.Timeout)
	if err != nil {
		return "", fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	// Overall deadline for handshake + session + command.
	deadline := time.Now().Add(o.Timeout)
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetDeadline(deadline)
	}

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return "", fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	defer conn.Close()
	client := ssh.NewClient(c, chans, reqs)

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	if err := session.Run(cmd); err != nil {
		return "", fmt.Errorf("ssh run %q: %w: %s", cmd, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (o Options) hostKeyCallback() ssh.HostKeyCallback {
	if !o.Strict {
		return ssh.InsecureIgnoreHostKey()
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if _, err := os.Stat(o.KnownHosts); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("unknown host key for %s and no known_hosts file yet: run 'pve-real-memory trust-host %s'", hostname, hostname)
			}
			return fmt.Errorf("known_hosts %s: %w", o.KnownHosts, err)
		}
		cb, err := knownhosts.New(o.KnownHosts)
		if err != nil {
			return fmt.Errorf("known_hosts %s: %w", o.KnownHosts, err)
		}
		if err := cb(hostname, remote, key); err != nil {
			return fmt.Errorf("host key verification failed for %s (%w); key rotated? run 'pve-real-memory forget-host %s' then 'trust-host %s'", hostname, err, hostname, hostname)
		}
		return nil
	}
}

// privateKeyAuth loads an unencrypted private key from a file. Every
// supported OpenSSH/PEM/PKCS#8 shape is handled by ParseRawPrivateKey;
// encrypted keys fail with an actionable message instead of being ignored.
func privateKeyAuth(path, passphrase string) (ssh.AuthMethod, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ssh key: %w", err)
	}
	fi, err := os.Stat(path)
	if err == nil && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("refusing world/group-readable ssh key %s (mode %o)", path, fi.Mode().Perm())
	}

	var key any
	if passphrase != "" {
		key, err = ssh.ParseRawPrivateKeyWithPassphrase(raw, []byte(passphrase))
	} else {
		key, err = ssh.ParseRawPrivateKey(raw)
	}
	if err != nil {
		var ppk *ssh.PassphraseMissingError
		if errors.As(err, &ppk) {
			return nil, fmt.Errorf("ssh key %s is passphrase-protected: set ssh.passphrase or use an unencrypted key", filepath.Base(path))
		}
		return nil, fmt.Errorf("parse ssh key %s: %w", path, err)
	}
	signer, ok := key.(ssh.Signer)
	if !ok {
		// ParseRawPrivateKey returns bare key material for modern key
		// types (e.g. *ed25519.PrivateKey); convert it.
		signer, err = ssh.NewSignerFromKey(key)
		if err != nil {
			return nil, fmt.Errorf("ssh key %s: unsupported key type %T", path, key)
		}
	}
	return ssh.PublicKeys(signer), nil
}

// TrustHost connects to host (verifying nothing), records the server host
// key into knownHosts, and reports the key fingerprint. It is the
// deliberate, visible alternative to disabling verification.
func TrustHost(o Options) (string, error) {
	if o.Port == 0 {
		o.Port = 22
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	var captured ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User:            o.User,
		Timeout:         o.Timeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error { captured = key; return nil },
	}
	if o.PrivateKey != "" {
		auth, err := privateKeyAuth(o.PrivateKey, o.Passphrase)
		if err != nil {
			return "", err
		}
		cfg.Auth = append(cfg.Auth, auth)
	}
	addr := net.JoinHostPort(o.Host, fmt.Sprint(o.Port))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil && captured == nil {
		return "", fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	if client != nil {
		client.Close()
	}
	if captured == nil {
		return "", errors.New("no host key captured")
	}
	line := knownhosts.Line([]string{addr}, captured)
	if err := appendKnownHost(o.KnownHosts, line); err != nil {
		return "", err
	}
	return ssh.FingerprintSHA256(captured), nil
}

func appendKnownHost(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), line) {
		return nil // already trusted
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\n", line)
	return err
}

// ForgetHost removes all entries for a host[:port] marker.
func ForgetHost(knownHostsPath, host string, port int) (int, error) {
	raw, err := os.ReadFile(knownHostsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	marker := knownhosts.Normalize(net.JoinHostPort(host, fmt.Sprint(port)))
	if port == 0 {
		marker = host
	}
	var kept []string
	removed := 0
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) > 0 && (fields[0] == marker || fields[0] == host) {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if removed == 0 {
		return 0, nil
	}
	tmp := knownHostsPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(kept, "\n")+"\n"), 0o644); err != nil {
		return 0, err
	}
	return removed, os.Rename(tmp, knownHostsPath)
}
