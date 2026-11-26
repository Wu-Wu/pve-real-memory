package sshc

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type testServer struct {
	addr       string
	listener   net.Listener
	meminfoOut string
}

func newTestServer(t *testing.T, clientAuthorizedKey ssh.PublicKey) *testServer {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() != "root" {
				return nil, fmt.Errorf("unknown user %s", meta.User())
			}
			if string(key.Marshal()) == string(clientAuthorizedKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("unauthorized key")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &testServer{
		addr:       ln.Addr().String(),
		listener:   ln,
		meminfoOut: "MemTotal: 2000000 kB\nMemAvailable: 1000000 kB\n",
	}

	go func() {
		for {
			ncc, err := ln.Accept()
			if err != nil {
				return
			}
			go handleConn(ncc, cfg, srv.meminfoOut)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return srv
}

func handleConn(ncc net.Conn, cfg *ssh.ServerConfig, meminfoOut string) {
	sshConn, chans, reqs, err := ssh.NewServerConn(ncc, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			return
		}
		go func() {
			defer ch.Close()
			for req := range chReqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				code := uint32(1)
				if err := ssh.Unmarshal(req.Payload, &payload); err == nil && strings.HasPrefix(payload.Command, "cat") {
					ch.Write([]byte(meminfoOut))
					code = 0
				}
				req.Reply(true, nil)
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{code}))
				return
			}
		}()
	}
}

func writeKey(t *testing.T, dir, name string, priv ed25519.PrivateKey, mode os.FileMode) (string, ssh.PublicKey) {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(priv, "prm-test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(block), mode); err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	return path, pub
}

func TestSSHRunAndHostTrust(t *testing.T) {
	dir := t.TempDir()
	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyPath, clientPub := writeKey(t, dir, "id", clientPriv, 0o600)
	srv := newTestServer(t, clientPub)

	host, portStr, _ := net.SplitHostPort(srv.addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	knownHosts := filepath.Join(dir, "known_hosts")
	base := Options{
		User: "root", Host: host, Port: port,
		PrivateKey: keyPath, Timeout: 5 * time.Second,
		KnownHosts: knownHosts,
	}

	// Strict on an empty known_hosts must refuse.
	strict := base
	strict.Strict = true
	if _, err := Run(strict, "cat /proc/meminfo"); err == nil {
		t.Fatal("unknown host key must be refused in strict mode")
	} else {
		t.Logf("strict refusal (expected): %v", err)
	}

	// Insecure mode works.
	out, err := Run(base, "cat /proc/meminfo")
	if err != nil {
		t.Fatalf("insecure run: %v", err)
	}
	if !strings.Contains(out, "MemTotal") {
		t.Errorf("out = %q", out)
	}

	// Explicit trust, then strict works.
	fp, err := TrustHost(base)
	if err != nil {
		t.Fatalf("TrustHost: %v", err)
	}
	if !strings.HasPrefix(fp, "SHA256:") {
		t.Errorf("fingerprint = %q", fp)
	}
	if _, err := Run(strict, "cat /proc/meminfo"); err != nil {
		t.Fatalf("strict run after trust: %v", err)
	}

	// Forget, strict refuses again.
	n, err := ForgetHost(knownHosts, base.Host, base.Port)
	if err != nil || n != 1 {
		t.Fatalf("ForgetHost = %d, %v", n, err)
	}
	if _, err := Run(strict, "cat /proc/meminfo"); err == nil {
		t.Fatal("strict must refuse after ForgetHost")
	}
}

func TestGroupReadableKeyRefused(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	keyPath, _ := writeKey(t, dir, "id", priv, 0o644)
	_, err := privateKeyAuth(keyPath, "")
	if err == nil || !strings.Contains(err.Error(), "readable") {
		t.Fatalf("err = %v", err)
	}
}

func TestWrongKeyFailsAuth(t *testing.T) {
	dir := t.TempDir()
	_, goodPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, goodPub := writeKey(t, dir, "good", goodPriv, 0o600)
	srv := newTestServer(t, goodPub)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	badKey, _ := writeKey(t, dir, "bad", otherPriv, 0o600)

	host, portStr, _ := net.SplitHostPort(srv.addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	_, err := Run(Options{User: "root", Host: host, Port: port, PrivateKey: badKey, Timeout: 5 * time.Second}, "cat /proc/meminfo")
	if err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("err = %v", err)
	}
}
