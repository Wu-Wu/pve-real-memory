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
   | `qga` | Linux, FreeBSD, Windows, Talos (detected → endpoint-based) | `guest-exec` via `/run/qemu-server/<vmid>.qga` (resync-framed, collision-safe) |
   | `ssh` | Linux, FreeBSD | `cat /proc/meminfo` / `sysctl` with strict host-key pinning, per-VM user/port/key |
   | `http` | Talos Linux | [`meminfo-service`](https://github.com/Wu-Wu/meminfo-service) extension endpoint `http://<node>:1027/meminfo` |
   Windows uses PowerShell `Get-CimInstance Win32_OperatingSystem`
   (`wmic` is gone from current Windows 11 builds).
   Linux math is `MemTotal − MemAvailable` — cache is not "used".
3. **Visibility** — `pvestatd` calls `vmstatus(full)`, so the GUI,
   `status/current` and RRD graphs all pick the corrected value up
   (within ~30 s, the pvestatd interval).

## Deploy

Upload the static binary (no dependencies) for your architecture and go.
Proxmox 9.2+ supports both amd64 and arm64; pick by `uname -m` —
`x86_64` → amd64, `aarch64` → arm64:

```sh
uname -m                                  # x86_64 or aarch64
curl -LO https://github.com/Wu-Wu/pve-real-memory/releases/latest/download/pve-real-memory-linux-amd64   # or -linux-arm64
install -m 0755 pve-real-memory-linux-* /usr/local/bin/pve-real-memory
/usr/local/bin/pve-real-memory --install
```

`--install` is a no-hassle round trip:

- writes `/etc/pve-real-memory/config.yaml` (sane defaults, auto-discovery
  on) — edits are picked up live, no restart;
- generates an ed25519 monitoring key and prints the `authorized_keys`
  line for SSH-monitored guests;
- applies the Perl patch, installs the systemd unit and an apt
  Post-Invoke hook, restarts `pvestatd`/`pvedaemon`;
- detects and tears down a legacy `proxmox-rmem` install automatically
  (its files are archived under `/var/lib/pve-real-memory/legacy-backup/`,
  nothing is restored over newer PVE files; the legacy **config stays in
  place** — saving it is `proxmox-rmem-helper`'s job, see below).

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
    os: freebsd         # linux | freebsd | talos | auto (qga only)
    host: 10.10.10.11
    user: monitor       # per-VM ssh user (root is often forbidden)
    port: 2022          # per-VM port; privateKey works too
  - vmid: 303
    method: http        # Talos node without QGA, pinned explicitly
    host: 192.168.2.30
```

### Talos guests

Talos is auto-discovered like any other VM **when the qemu-guest-agent
system extension is present**: `guest-get-osinfo` is mapped to
`os: talos` (real agents report only `kernel-release: X-talos`, without
`id`/`name`), collection then goes to the
[`meminfo-service`](https://github.com/Wu-Wu/meminfo-service)
extension endpoint at `http://<agent-reported-IPv4>:1027/meminfo`
(the IP is cached with the discovery entry; a `host:` on the source pins
it explicitly). The endpoint is the **only** Talos channel — the guest
has no shell and the agent allows no `guest-exec`, which is exactly why
the endpoint exists. When it is unreachable the daemon logs the error
and writes nothing: the hook's TTL leaves the host view in place.
`guest-get-network-interfaces` is best effort; deployments that block
that RPC pin the address with `host:`. Nodes without the agent are
declared with `method: http` (SSH is not usable there either).

> Note: agent builds differ in supported RPCs — real-world examples:
> Talos 1.13 reports only `kernel-release` (detected via the `-talos`
> suffix); some Windows agent builds lack `guest-get-osinfo` entirely
> (they need an explicit `os: windows` on the source). Auto-discovery
> skips guests whose identity the agent cannot report; the skipped VM
> is visible with `logLevel: debug`.

### SSH host keys
Strict verification is on by default. Pin a guest once:

```sh
pve-real-memory trust-host 10.10.10.11        # records the key, prints fingerprint
pve-real-memory forget-host 10.10.10.11       # after a deliberate key rotation
```

The fetch error for an unknown key tells you exactly this command.
Group/world-readable private keys and passphrase-protected keys are
refused with an actionable message rather than skipped.

Each `ssh` source may carry its own `user:` / `port:` / `privateKey:` —
useful where root login is forbidden: point the source at a dedicated
monitor user (both collectors, `cat /proc/meminfo` and the FreeBSD
`sysctl`s, are world-readable).

### Keeping the patch across Proxmox upgrades

`/etc/apt/apt.conf.d/99-pve-real-memory` re-applies the hook after any
apt transaction that overwrote `QemuServer.pm`; the daemon's watchdog is
a second line of defense (locked, `perl -c`-verified, auto-reverts on a
failed check). The hook never returns non-zero — apt can't break because
of it.

### proxmox-rmem-helper

A second, tiny binary shipped from this repo saves and restores the
legacy `proxmox-rmem` configuration around a migration. The installer
deliberately leaves `/etc/proxmox-rmem/` in place, so you own the
safety copy:

```sh
proxmox-rmem-helper --backup    # BEFORE installing pve-real-memory,
                                # if you may want to roll back later
proxmox-rmem-helper --restore   # when rolling back: unpacks the saved
                                # directory (survives a purge of the
                                # old package; restart proxmox-rmem
                                # afterwards)
```

`--backup` tars the **whole** `/etc/proxmox-rmem` directory —
`config.json`, the `id_rsa_monitor` key (losing it means redeploying
authorized_keys on every guest) and the install stamp — into
`/var/backups/proxmox-rmem/etc-proxmox-rmem.tar.gz` (mode 0600, since
it holds a private key); `--restore` unpacks it back with the original
permission bits. The PVE patch itself is deliberately not part of any
snapshot: copying `QemuServer.pm` over a possibly-newer installed file
is the old `.bak` downgrade defect — each project's installer applies
its own patch on the live module.
If the source is missing (no config dir / no archive) each prints
"nothing to do" and exits 0 — that is a success, not an error. All
messages go to stdout; a successful run reports paths, file/byte
counts and a sha256 prefix, and notes whether it replaced an existing
backup/config. Writes are atomic (temp+rename); archive entries with
unsafe paths are refused.

## Development

Everything runs in local docker, no host Go toolchain needed:

```sh
make test    # go test ./... inside golang:1.26 (perl -c tests included)
make vet
make build   # static linux/amd64 + linux/arm64 binaries:
             #   bin/pve-real-memory-linux-{amd64,arm64}
             #   bin/proxmox-rmem-helper-linux-{amd64,arm64}
```

Tested against the `vmstatus()` source of Proxmox qemu-server 9.2 and
9.3 (fixtures in `internal/patch/testdata/`); the patcher refuses to
touch a file it can't anchor with a clear error instead of guessing.

### Versioning

There is no version file to keep in sync: every build stamps
`git describe --tags --always` (`-dirty` suffix when the tree is
unclean) into both binaries via ldflags. It is shown by
`pve-real-memory version` / `--version`, as the first line of
`pve-real-memory status`, in the daemon's startup log, and via
`go version -m <binary>`. Cutting a release is one annotated tag:

```sh
make tag TAG=v1.0.0    # git tag -a v1.0.0
make build             # bin/*-linux-* now report "v1.0.0"
```

`make version` prints what the next build will embed.

## Security notes

- Override store is a root-only `0700` runtime dir with `0600` files,
  written atomically — no `/tmp`, no symlink race, no forged values.
- SSH: strict host keys by default, key permissions enforced, overall
  deadline on every dial/handshake/command.
- HTTP: TLS verified unless you opt in per source; response size capped;
  malformed/negative/float payloads rejected, never written as overrides.
- The binary itself only needs root for lifecycle verbs; the patch is
  explicit, marker-bounded and reversible to the byte.
