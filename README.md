# pve-real-memory

Real guest memory usage in Proxmox VE — for VMs that don't (or can't)
report it through the balloon driver: FreeBSD/OPNsense/pfSense, Windows
without the balloon service, Talos nodes, anything with ballooning
disabled.

## Why

Proxmox VE 9 shows the **host-side** view of VM memory (`memhost`
accounting) whenever a guest doesn't report its own usage through the
ballooning device. That view includes page cache charged to the VM and
can show absurd values — even over 100%. Since PVE 9 keeps the host view
in a separate `memhost` field, replacing the reported `mem` with what
the guest actually uses loses nothing.

## How it works

One static Go binary per Proxmox node plus one YAML config:

1. **Patch** — `--install` inserts a small marker-bounded block into
   `PVE::QemuServer::vmstatus()` (verified with `perl -c`, backed up,
   exactly reversible). The block replaces a VM's `mem` with the content
   of `/run/pve-real-memory/<vmid>.mem` when that file is fresh (≤ 120
   s old). Root-only `0700` directory, atomic writes — stale data ages
   out on its own, nothing freezes.
2. **Daemon** — fetches real usage from every VM (5 s cadence, parallel)
   over the best transport:
   | transport | guests | what runs where |
   |---|---|---|
   | `qga` | Linux, FreeBSD, Windows | `guest-exec` via `/run/qemu-server/<vmid>.qga` (resync-framed, collision-safe) |
   | `ssh` | Linux, FreeBSD | `cat /proc/meminfo` / `sysctl` with strict host-key pinning |
   | `http` | Talos Linux | [`meminfo-service`](https://github.com/Wu-Wu/meminfo-service) extension endpoint `http://<node>:1027/meminfo` |
   Windows uses PowerShell `Get-CimInstance Win32_OperatingSystem`
   (`wmic` is gone from current Windows 11 builds).
   Linux math is `MemTotal − MemAvailable` — cache is not "used".
3. **Visibility** — `pvestatd` calls `vmstatus(full)`, so the GUI,
   `status/current` and RRD graphs all pick the corrected value up
   (within ~30 s, the pvestatd interval).

## Deploy

Upload the binary (static, no dependencies, amd64) and go:

```sh
curl -LO https://github.com/Wu-Wu/pve-real-memory/releases/latest/download/pve-real-memory
install -m 0755 pve-real-memory /usr/local/bin/
/usr/local/bin/pve-real-memory --install
```

`--install` is a no-hassle round trip:

- writes `/etc/pve-real-memory/config.yaml` (sane defaults, auto-discovery
  on) — edits are picked up live, no restart;
- generates an ed25519 monitoring key and prints the `authorized_keys`
  line for SSH-monitored guests;
- applies the Perl patch, installs the systemd unit and an apt
  Post-Invoke hook, restarts `pvestatd`/`pvedaemon`;
- detects and migrates a legacy `proxmox-rmem` install away
  automatically (its files are archived under
  `/var/lib/pve-real-memory/legacy-backup/`, nothing is restored over
  newer PVE files).

Check it:

```sh
pve-real-memory doctor          # config/keys/hooks, read-only
pve-real-memory once            # one fetch pass, per-VM report
pve-real-memory status          # patch/service/overrides
journalctl -u pve-real-memory -f
```

Remove it:

```sh
pve-real-memory --uninstall     # reverses patch/unit/hook, keeps config
pve-real-memory --uninstall --purge
```

## Configuration

`/etc/pve-real-memory/config.yaml`, hot-reloaded every cycle:

```yaml
interval: 5s
maxParallel: 8
logLevel: info

autoDiscovery:          # every VM with a QGA socket on this node
  enabled: true
  scanInterval: 60s
  exclude: []

ssh:
  user: root
  privateKey: /etc/pve-real-memory/id_ed25519
  knownHosts: /var/lib/pve-real-memory/known_hosts
  strictHostKeys: true  # never silently trust; see below
  timeout: 10s

sources:                # explicit wins over auto-discovery
  - vmid: 101
    method: ssh
    os: freebsd         # linux | freebsd | auto (qga only)
    host: 10.10.10.11
  - vmid: 303
    method: http        # Talos node running the meminfo-service extension
    host: 192.168.2.30
```

### SSH host keys

Strict verification is on by default. Pin a guest once:

```sh
pve-real-memory trust-host 10.10.10.11        # records the key, prints fingerprint
pve-real-memory forget-host 10.10.10.11       # after a deliberate key rotation
```

The fetch error for an unknown key tells you exactly this command.
Group/world-readable private keys and passphrase-protected keys are
refused with an actionable message rather than skipped.

### Keeping the patch across Proxmox upgrades

`/etc/apt/apt.conf.d/99-pve-real-memory` re-applies the hook after any
apt transaction that overwrote `QemuServer.pm`; the daemon's watchdog is
a second line of defense (locked, `perl -c`-verified, auto-reverts on a
failed check). The hook never returns non-zero — apt can't break because
of it.

## Development

Everything runs in local docker, no host Go toolchain needed:

```sh
make test    # go test ./... inside golang:1.26 (perl -c tests included)
make vet
make build   # static linux/amd64 binary -> ./bin/pve-real-memory
```

Tested against the `vmstatus()` source of Proxmox qemu-server 9.2 and
9.3 (fixtures in `internal/patch/testdata/`); the patcher refuses to
touch a file it can't anchor with a clear error instead of guessing.

## Security notes

- Override store is a root-only `0700` runtime dir with `0600` files,
  written atomically — no `/tmp`, no symlink race, no forged values.
- SSH: strict host keys by default, key permissions enforced, overall
  deadline on every dial/handshake/command.
- HTTP: TLS verified unless you opt in per source; response size capped;
  malformed/negative/float payloads rejected, never written as overrides.
- The binary itself only needs root for lifecycle verbs; the patch is
  explicit, marker-bounded and reversible to the byte.
