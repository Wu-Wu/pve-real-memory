// Package qga talks to the in-guest qemu-guest-agent through the per-VM
// unix socket QEMU exposes at /run/qemu-server/<vmid>.qga.
//
// Wire protocol notes (qga/commands.c and PVE::QMPClient as references):
//   - The guest agent is a plain JSON-lines request/response service with
//     no request IDs and no greeting. A connection can carry stale bytes:
//     a response to a previously timed-out command, or async notifications.
//   - The official resync mechanism is guest-sync-delimited: the agent
//     answers it with the 0xFF sentinel byte immediately before the JSON
//     reply. Everything read before the sentinel is garbage to be dropped.
//     PVE does exactly this on every command; so do we.
//   - QEMU serves the chardev with a single client slot, so a connect can
//     collide with pvesh/qm/balloon tooling. Collisions surface as an
//     immediate EOF and are retried once.
package qga

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// sentinel is the guest-sync-delimited resync byte.
const sentinel = 0xff

// Runner executes commands through QGA sockets.
type Runner struct {
	SocketDir string
	Timeout   time.Duration // per operation (connect through last response)
	// PollInterval is the guest-exec-status polling cadence.
	PollInterval time.Duration

	ids atomic.Int64

	muMu sync.Mutex
	mus  map[int]*sync.Mutex // serialize sessions per VM
}

// NewRunner builds a Runner with sane defaults.
func NewRunner(socketDir string, timeout time.Duration) *Runner {
	return &Runner{
		SocketDir:    socketDir,
		Timeout:      timeout,
		PollInterval: 100 * time.Millisecond,
		mus:          map[int]*sync.Mutex{},
	}
}

func (r *Runner) lock(vmid int) *sync.Mutex {
	r.muMu.Lock()
	defer r.muMu.Unlock()
	m, ok := r.mus[vmid]
	if !ok {
		m = &sync.Mutex{}
		r.mus[vmid] = m
	}
	return m
}

// SocketPath is the QGA socket for a VM.
func SocketPath(dir string, vmid int) string {
	return filepath.Join(dir, strconv.Itoa(vmid)+".qga")
}

// Running reports whether the VM's QGA socket exists (VM running with the
// agent device enabled).
func Running(dir string, vmid int) bool {
	fi, err := os.Lstat(SocketPath(dir, vmid))
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// ListVMIDs enumerates VMs with a QGA socket on this node.
func ListVMIDs(dir string) ([]int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []int
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".qga") {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".qga"))
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// session is one connected agent conversation.
type session struct {
	conn net.Conn
	r    *bufio.Reader
	run  *Runner
}

// dial opens the agent socket with one collision retry.
func (r *Runner) dial(vmid int, deadline time.Time) (*session, error) {
	path := SocketPath(r.SocketDir, vmid)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := net.Dial("unix", path)
		if err != nil {
			lastErr = err
			if !retryableDial(err) {
				return nil, fmt.Errorf("qga dial vmid %d: %w", vmid, err)
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return &session{conn: conn, r: bufio.NewReader(conn), run: r}, nil
	}
	return nil, fmt.Errorf("qga dial vmid %d: %w", vmid, lastErr)
}

func retryableDial(err error) bool {
	var se *net.OpError
	if errors.As(err, &se) {
		if se.Err == os.ErrPermission {
			return false
		}
		return true // refused usually means chardev slot busy; retry once
	}
	return true
}

func (s *session) setDeadline(t time.Time) { s.conn.SetDeadline(t) }

func (s *session) close() { s.conn.Close() }

// closedConn reports the shapes a peer disconnect takes across platforms:
// the usual "chardev slot held by another client" collision.
func closedConn(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed)
}

// cmd sends one command and returns its reply, resyncing first.
func (s *session) cmd(execute string, arguments map[string]any, deadline time.Time) (json.RawMessage, error) {
	s.setDeadline(deadline)

	// 1. Resync: drain stale bytes, wait for the sentinel reply.
	id := s.run.ids.Add(1)
	syncMsg, _ := json.Marshal(map[string]any{
		"execute":   "guest-sync-delimited",
		"arguments": map[string]any{"id": id},
	})
	if _, err := s.conn.Write(append(syncMsg, '\n')); err != nil {
		if closedConn(err) {
			return nil, errors.New("agent closed the connection (is qga running in the guest, or is the socket held by another client?)")
		}
		return nil, fmt.Errorf("write sync: %w", err)
	}
	if err := s.readSyncAck(id, deadline); err != nil {
		return nil, err
	}

	// 2. The real command.
	msg := map[string]any{"execute": execute}
	if arguments != nil {
		msg["arguments"] = arguments
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if _, err := s.conn.Write(append(body, '\n')); err != nil {
		return nil, fmt.Errorf("write %s: %w", execute, err)
	}

	// 3. Read the reply, skipping any async line.
	for {
		line, err := s.readLine(deadline)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", execute, err)
		}
		var probe struct {
			Return json.RawMessage `json:"return"`
			Error  *struct {
				Class string `json:"class"`
				Desc  string `json:"desc"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			continue // not JSON we understand; skip
		}
		if probe.Error != nil {
			return nil, fmt.Errorf("%s failed: %s: %s", execute, probe.Error.Class, probe.Error.Desc)
		}
		if probe.Return != nil {
			return probe.Return, nil
		}
		// async notification, keep reading
	}
}

// readSyncAck consumes bytes up to and including the sentinel, then checks
// the JSON line that follows.
func (s *session) readSyncAck(id int64, deadline time.Time) error {
	sawSentinel := false
	for {
		if time.Now().After(deadline) {
			return errors.New("resync timeout: agent unresponsive")
		}
		b, err := s.r.ReadByte()
		if err != nil {
			if closedConn(err) {
				return errors.New("agent closed the connection (is qga running in the guest, or is the socket held by another client?)")
			}
			return fmt.Errorf("resync: %w", err)
		}
		if b == sentinel {
			sawSentinel = true
			break
		}
		// stale byte, drop it
	}
	if !sawSentinel {
		return errors.New("resync: no sentinel")
	}
	line, err := s.readLine(deadline)
	if err != nil {
		return fmt.Errorf("resync ack: %w", err)
	}
	var ack struct {
		Return int64 `json:"return"`
	}
	if err := json.Unmarshal([]byte(line), &ack); err != nil {
		return fmt.Errorf("resync ack %q: %w", line, err)
	}
	if ack.Return != id {
		return fmt.Errorf("resync mismatch: got %d want %d", ack.Return, id)
	}
	return nil
}

func (s *session) readLine(deadline time.Time) (string, error) {
	s.setDeadline(deadline)
	line, err := s.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// Call issues a single agent command (diagnostics; most users want Exec).
func (r *Runner) Call(vmid int, execute string, args map[string]any) (json.RawMessage, error) {
	mu := r.lock(vmid)
	mu.Lock()
	defer mu.Unlock()

	deadline := time.Now().Add(r.Timeout)
	s, err := r.dial(vmid, deadline)
	if err != nil {
		return nil, err
	}
	defer s.close()
	return s.cmd(execute, args, deadline)
}

// Exec runs a command in the guest and returns stdout; the whole operation
// (dial through last status poll) is bounded by Runner.Timeout.
func (r *Runner) Exec(vmid int, path string, args []string) (string, error) {
	mu := r.lock(vmid)
	mu.Lock()
	defer mu.Unlock()

	deadline := time.Now().Add(r.Timeout)
	s, err := r.dial(vmid, deadline)
	if err != nil {
		return "", err
	}
	defer s.close()

	argsMap := map[string]any{
		"path":           path,
		"capture-output": true,
	}
	if len(args) > 0 {
		argsMap["arg"] = args
	}

	raw, err := s.cmd("guest-exec", argsMap, deadline)
	if err != nil {
		return "", err
	}
	var started struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.PID == 0 {
		return "", fmt.Errorf("guest-exec returned no pid: %s", raw)
	}

	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("guest-exec vmid %d pid %d: timed out", vmid, started.PID)
		}
		raw, err := s.cmd("guest-exec-status", map[string]any{"pid": started.PID}, deadline)
		if err != nil {
			return "", err
		}
		var st struct {
			Exited   *bool  `json:"exited"`
			ExitCode *int   `json:"exitcode"`
			OutData  string `json:"out-data"`
			ErrData  string `json:"err-data"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return "", fmt.Errorf("bad exec-status: %w", err)
		}
		if st.Exited != nil && *st.Exited {
			code := -1
			if st.ExitCode != nil {
				code = *st.ExitCode
			}
			if code != 0 {
				stderr := decodeB64(st.ErrData)
				return "", fmt.Errorf("command %s exited %d: %s", path, code, strings.TrimSpace(stderr))
			}
			return decodeB64(st.OutData), nil
		}
		time.Sleep(r.PollInterval)
	}
}

func decodeB64(s string) string {
	if s == "" {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return s
	}
	return string(b)
}

// OSInfo is the subset of guest-get-osinfo we use.
type OSInfo struct {
	Name          string `json:"name"`
	ID            string `json:"id"`
	KernelRelease string `json:"kernel-release"`
	Variant       string `json:"variant"`
	VariantID     string `json:"variant-id"`
}

// Detect maps agent-reported identity to our OS vocabulary; "" when the
// agent did not report anything usable.
func (o *OSInfo) Detect() string {
	if o == nil {
		return ""
	}
	name := strings.ToLower(o.Name)
	id := strings.ToLower(o.ID)
	kernel := strings.ToLower(o.KernelRelease)

	if strings.Contains(name, "windows") || strings.Contains(name, "microsoft") || strings.Contains(id, "windows") {
		return "windows"
	}
	// Talos: real agents (v1.13) report ONLY kernel-* fields — no id,
	// no name — with the kernelRelease carrying the "-talos" suffix.
	// It is Linux-kernel based but needs its own collection path
	// (meminfo-service endpoint preferred).
	if strings.Contains(id, "talos") || strings.Contains(name, "talos") || strings.Contains(kernel, "talos") {
		return "talos"
	}
	for _, bsd := range []string{"freebsd", "openbsd", "netbsd", "opnsense", "pfsense"} {
		if strings.Contains(id, bsd) || strings.Contains(name, bsd) || strings.Contains(kernel, bsd) ||
			strings.Contains(strings.ToLower(o.VariantID), bsd) || strings.Contains(strings.ToLower(o.Variant), bsd) {
			return "freebsd"
		}
	}
	if name != "" || id != "" {
		return "linux"
	}
	return ""
}

// GetIPv4 returns the first non-loopback IPv4 the agent reports, used to
// reach Talos guests' HTTP endpoints when the source has no explicit
// host configured.
func (r *Runner) GetIPv4(vmid int) (string, error) {
	mu := r.lock(vmid)
	mu.Lock()
	defer mu.Unlock()

	deadline := time.Now().Add(r.Timeout)
	s, err := r.dial(vmid, deadline)
	if err != nil {
		return "", err
	}
	defer s.close()

	raw, err := s.cmd("guest-get-network-interfaces", map[string]any{"ignored-virtnames": []string{"lo"}}, deadline)
	if err != nil {
		return "", err
	}
	var ifaces []struct {
		Name        string `json:"name"`
		IPAddresses []struct {
			IPAddress     string `json:"ip-address"`
			IPAddressType string `json:"ip-address-type"`
		} `json:"ip-addresses"`
	}
	if err := json.Unmarshal(raw, &ifaces); err != nil {
		return "", fmt.Errorf("bad interfaces reply: %w", err)
	}
	for _, ifc := range ifaces {
		if ifc.Name == "lo" {
			continue
		}
		for _, addr := range ifc.IPAddresses {
			if addr.IPAddressType == "ipv4" && addr.IPAddress != "" && !strings.HasPrefix(addr.IPAddress, "127.") {
				return addr.IPAddress, nil
			}
		}
	}
	return "", fmt.Errorf("agent reports no routable IPv4 address")
}

// GetOSInfo queries the agent for guest identity.
func (r *Runner) GetOSInfo(vmid int) (*OSInfo, error) {
	mu := r.lock(vmid)
	mu.Lock()
	defer mu.Unlock()

	deadline := time.Now().Add(r.Timeout)
	s, err := r.dial(vmid, deadline)
	if err != nil {
		return nil, err
	}
	defer s.close()

	raw, err := s.cmd("guest-get-osinfo", nil, deadline)
	if err != nil {
		return nil, err
	}
	var info OSInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("bad osinfo: %w", err)
	}
	return &info, nil
}
