// pve-real-memory reports real guest memory usage to Proxmox VE by patching
// PVE::QemuServer::vmstatus() to read per-VM override files, and maintaining
// those files with real values fetched over QGA, SSH or HTTP.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	args := os.Args[1:]

	cmd := "run"
	if len(args) > 0 {
		cmd = args[0]
	}

	fmt.Fprintf(os.Stderr, "%s %s: command %q is not implemented yet\n", os.Args[0], version, cmd)
	os.Exit(1)
}
