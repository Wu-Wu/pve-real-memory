// proxmox-rmem-helper saves and restores the legacy proxmox-rmem
// configuration around a pve-real-memory migration. Every message goes
// to stdout; "source not found" is a successful no-op, not an error.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Wu-Wu/pve-real-memory/internal/rmemconf"
)

var version = "dev"

const usage = `proxmox-rmem-helper — save/restore the proxmox-rmem configuration

usage:
  proxmox-rmem-helper --backup    save /etc/proxmox-rmem (config.json,
                                ssh keys, install stamp) as a single
                                archive: /var/backups/proxmox-rmem/etc-proxmox-rmem.tar.gz
  proxmox-rmem-helper --restore   unpack the archive back into
                                /etc/proxmox-rmem (dir is created if
                                the old package was purged)
  proxmox-rmem-helper --version   print version

Run --backup before installing pve-real-memory if you may want to roll
back later; --restore is useful after a purge of the old package.
The PVE patch itself is not part of the backup — the respective
installer applies it on top of the live (possibly newer) PVE files.
`

func main() {
	flag.Usage = func() { fmt.Print(usage) }
	backup := flag.Bool("backup", false, "save the current proxmox-rmem config")
	restore := flag.Bool("restore", false, "restore the proxmox-rmem config from backup")
	ver := flag.Bool("version", false, "print version")
	flag.Parse()

	if *ver {
		fmt.Printf("proxmox-rmem-helper %s\n", version)
		return
	}

	n := 0
	for _, b := range []bool{*backup, *restore} {
		if b {
			n++
		}
	}
	if n != 1 {
		fmt.Print(usage)
		os.Exit(2)
	}

	if os.Geteuid() != 0 {
		fmt.Println("proxmox-rmem-helper: must run as root (reads/writes /etc and /var/backups)")
		os.Exit(1)
	}

	p := rmemconf.Default()
	var (
		msg  string
		noop bool
		err  error
	)
	if *backup {
		msg, noop, err = p.Backup()
	} else {
		msg, noop, err = p.Restore()
	}
	if err != nil {
		fmt.Printf("proxmox-rmem-helper: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(msg)
	if noop {
		os.Exit(0) // "nothing to do" is a success per the tool's contract
	}
}
