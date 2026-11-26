package svc

// UnitFile renders the systemd unit. RuntimeDirectory gives us a fresh
// root-only /run/pve-real-memory on every start; UMask keeps override
// files and any state written by the daemon private.
func UnitFile(binaryPath, args string) string {
	return `[Unit]
Description=pve-real-memory — real guest memory reporting for Proxmox VE
Documentation=https://github.com/Wu-Wu/pve-real-memory
After=network-online.target pve-cluster.service pvedaemon.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + binaryPath + ` ` + args + `
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=15
RuntimeDirectory=pve-real-memory
RuntimeDirectoryMode=0700
StateDirectory=pve-real-memory
StateDirectoryMode=0755
UMask=0077
OOMPolicy=continue

[Install]
WantedBy=multi-user.target
`
}

// AptHook keeps the vmstatus patch alive across Proxmox apt upgrades.
// repair --hook never exits non-zero, so apt can never fail because of
// this hook.
func AptHook(binaryPath string) string {
	return `// pve-real-memory: re-apply the vmstatus memory patch after Proxmox upgrades
DPkg::Post-Invoke { "` + binaryPath + ` repair --hook"; };
`
}

// ConfigTemplate is written on first install.
const ConfigTemplate = `# pve-real-memory — real guest memory reporting for Proxmox VE.
# The daemon re-reads this file every cycle; edits apply without restart.

interval: 5s          # fetch cadence for every source
maxParallel: 8        # concurrent fetches per cycle
logLevel: info        # debug | info | warn | error

autoDiscovery:
  enabled: true       # watch every VM that has a QGA socket on this node
  scanInterval: 60s
  exclude: []         # VMIDs auto-discovery must skip, e.g. [101, "202"]

qga:
  socketDir: /run/qemu-server
  execTimeout: 10s

ssh:
  user: root
  port: 22
  privateKey: /etc/pve-real-memory/id_ed25519
  knownHosts: /var/lib/pve-real-memory/known_hosts
  strictHostKeys: true  # refuse unknown host keys; use: pve-real-memory trust-host <vm-host>
  timeout: 10s

http:
  timeout: 10s

# Explicit sources (method: qga | ssh | http; os: auto | linux | freebsd | windows).
# ssh/http sources need host:; http also takes url:/token:/headers: (Talos nodes
# run the meminfo-service extension and answer on http://<host>:1027/meminfo).
#sources:
#  - vmid: 101
#    method: ssh
#    os: freebsd
#    host: 10.10.10.11
#  - vmid: 303
#    method: http
#    host: 192.168.2.30
`
