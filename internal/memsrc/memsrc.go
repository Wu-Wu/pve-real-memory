// Package memsrc contains the pure parsers that turn guest-provided text
// into used-bytes, plus the exact commands used to obtain that text.
// Transports (QGA/SSH/HTTP) live elsewhere; this package is what makes the
// numbers correct.
package memsrc

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ParseLinuxMeminfo computes MemTotal - MemAvailable (values are kB).
// MemAvailable is mandatory: without it we would re-introduce the
// cache-is-used-memory lie this project exists to fix.
func ParseLinuxMeminfo(content string) (int64, error) {
	var total, avail int64 = -1, -1
	for _, line := range strings.Split(content, "\n") {
		if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			n, err := parseKBField(v)
			if err != nil {
				return 0, fmt.Errorf("MemTotal: %w", err)
			}
			total = n
		} else if v, ok := strings.CutPrefix(line, "MemAvailable:"); ok {
			n, err := parseKBField(v)
			if err != nil {
				return 0, fmt.Errorf("MemAvailable: %w", err)
			}
			avail = n
		}
	}
	if total < 0 {
		return 0, errors.New("MemTotal not found")
	}
	if avail < 0 {
		return 0, errors.New("MemAvailable not found (kernel too old?)")
	}
	used := total - avail
	if used < 0 {
		return 0, fmt.Errorf("inconsistent meminfo: MemAvailable %d kB > MemTotal %d kB", avail, total)
	}
	return used * 1024, nil
}

func parseKBField(s string) (int64, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, errors.New("empty value")
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("negative value %d", n)
	}
	return n, nil
}

// ParseFreeBSDSysctl consumes the output of
//
//	sysctl -n vm.stats.vm.v_active_count vm.stats.vm.v_wire_count vm.stats.vm.v_page_size
//
// and computes (active + wired) * page_size.
func ParseFreeBSDSysctl(content string) (int64, error) {
	fields := strings.Fields(content)
	if len(fields) != 3 {
		return 0, fmt.Errorf("expected 3 sysctl values, got %d: %q", len(fields), content)
	}
	v := make([]int64, 3)
	for i, f := range fields {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad sysctl value %q", f)
		}
		v[i] = n
	}
	return (v[0] + v[1]) * v[2], nil
}

// ParseWindowsCIM consumes compact JSON from
//
//	Get-CimInstance Win32_OperatingSystem | Select TotalVisibleMemorySize,FreePhysicalMemory | ConvertTo-Json -Compress
//
// values are kB (may arrive as JSON numbers or strings).
func ParseWindowsCIM(content string) (int64, error) {
	var obj struct {
		TotalVisibleMemorySize json.Number `json:"TotalVisibleMemorySize"`
		FreePhysicalMemory     json.Number `json:"FreePhysicalMemory"`
	}
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(content)))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return 0, fmt.Errorf("powershell output is not JSON: %w", err)
	}
	total, err := kbNumber(obj.TotalVisibleMemorySize, "TotalVisibleMemorySize")
	if err != nil {
		return 0, err
	}
	free, err := kbNumber(obj.FreePhysicalMemory, "FreePhysicalMemory")
	if err != nil {
		return 0, err
	}
	if free > total {
		return 0, fmt.Errorf("inconsistent CIM: FreePhysicalMemory %d > TotalVisibleMemorySize %d", free, total)
	}
	return (total - free) * 1024, nil
}

func kbNumber(n json.Number, field string) (int64, error) {
	if n.String() == "" {
		return 0, fmt.Errorf("%s missing", field)
	}
	v, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%s = %q is not a non-negative integer", field, n.String())
	}
	return v, nil
}

// ParseMeminfoJSON consumes the meminfo-service protocol:
//
//	{"total":..., "avail":..., "used":...}   (bytes)
//
// "used" wins when present; otherwise total-avail. All-zero / missing /
// negative values are rejected, and JSON must be an object (an endpoint
// answering with a list or a bare number is a configuration error, not a
// zero reading).
func ParseMeminfoJSON(content string) (int64, error) {
	var obj map[string]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(content)))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return 0, fmt.Errorf("endpoint response is not a JSON object: %w", err)
	}
	get := func(key string) (int64, bool, error) {
		raw, ok := obj[key]
		if !ok {
			return 0, false, nil
		}
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			// also accept "123" strings
			var s string
			if err2 := json.Unmarshal(raw, &s); err2 != nil {
				return 0, true, fmt.Errorf("%s is not a number", key)
			}
			n = json.Number(s)
		}
		v, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return 0, true, fmt.Errorf("%s = %q is not an integer", key, n.String())
		}
		if v < 0 {
			return 0, true, fmt.Errorf("%s is negative", key)
		}
		return v, true, nil
	}

	used, hasUsed, err := get("used")
	if err != nil {
		return 0, err
	}
	total, hasTotal, err := get("total")
	if err != nil {
		return 0, err
	}
	avail, hasAvail, err := get("avail")
	if err != nil {
		return 0, err
	}

	// Priority: explicit used, then total-avail, then a reported zero.
	if hasUsed && used > 0 {
		return used, nil
	}
	if hasTotal && hasAvail {
		if total == 0 {
			return 0, errors.New("endpoint reports total=0: broken payload")
		}
		if avail > total {
			return 0, fmt.Errorf("avail %d > total %d", avail, total)
		}
		return total - avail, nil
	}
	if hasUsed {
		return used, nil // used == 0 on an idle node is legitimate
	}
	return 0, errors.New("endpoint payload has neither usable used nor total+avail")
}

// Command definitions shared by SSH and QGA transports.

// LinuxCommand is the in-guest command for Linux usage.
var LinuxCommand = struct {
	Path string
	Args []string
}{"cat", []string{"/proc/meminfo"}}

// FreeBSDCommand is the in-guest command for FreeBSD usage.
var FreeBSDCommand = struct {
	Path string
	Args []string
}{"sysctl", []string{"-n",
	"vm.stats.vm.v_active_count",
	"vm.stats.vm.v_wire_count",
	"vm.stats.vm.v_page_size"}}

// windowsPSCommand renders guest RAM usage as one compact JSON object.
const windowsPSCommand = `$os = Get-CimInstance Win32_OperatingSystem; ` +
	`[pscustomobject]@{TotalVisibleMemorySize=$os.TotalVisibleMemorySize; FreePhysicalMemory=$os.FreePhysicalMemory} | ConvertTo-Json -Compress`

// WindowsCommand is the in-guest command for Windows usage (wmic is
// removed in current Windows 11 builds, CIM via PowerShell is the
// supported path).
var WindowsCommand = struct {
	Path string
	Args []string
}{"powershell.exe", []string{"-NoProfile", "-NonInteractive", "-Command", windowsPSCommand}}

// SSHCommand returns the remote shell line for an OS.
func SSHCommand(os string) (string, error) {
	switch os {
	case "linux":
		return "cat /proc/meminfo", nil
	case "freebsd":
		return "sysctl -n vm.stats.vm.v_active_count vm.stats.vm.v_wire_count vm.stats.vm.v_page_size", nil
	}
	return "", fmt.Errorf("no ssh memory command for os %q", os)
}

// ParseForOS parses transport output for the given OS vocabulary.
func ParseForOS(os, out string) (int64, error) {
	switch os {
	case "linux":
		return ParseLinuxMeminfo(out)
	case "freebsd":
		return ParseFreeBSDSysctl(out)
	case "windows":
		return ParseWindowsCIM(out)
	}
	return 0, fmt.Errorf("unknown os %q", os)
}
