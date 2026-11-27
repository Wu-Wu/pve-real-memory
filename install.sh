#!/usr/bin/env bash
#
# pve-real-memory installer — the documented manual deploy flow in one
# shot:
#
#   bash -c "$(curl -fsSL https://raw.githubusercontent.com/Wu-Wu/pve-real-memory/main/install.sh)"
#
# Steps (same as the README):
#   1. determine the architecture (uname -m)
#   2. download the matching binary from the latest release
#   3. install it into /usr/local/bin
#   4. run the self-install (--install: config, key, patch, systemd, apt hook)
#
set -euo pipefail

say() { echo "[install] $*"; }

# 0. --install writes to /etc and drives systemctl
if [ "$(id -u)" -ne 0 ]; then
    say "must run as root"
    exit 1
fi

# 1. determine the architecture
case "$(uname -m)" in
    x86_64)         ARCH=amd64 ;;
    aarch64|arm64)  ARCH=arm64 ;;
    *)
        say "unsupported architecture: $(uname -m) (need x86_64 or aarch64)"
        exit 1
        ;;
esac
say "architecture: $(uname -m) -> linux/$ARCH"

# 2. download the binary of the latest release
BIN="pve-real-memory-linux-$ARCH"
URL="https://github.com/Wu-Wu/pve-real-memory/releases/latest/download/$BIN"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
say "downloading $URL"
curl -fsSL --retry 3 --connect-timeout 10 -o "$TMP/$BIN" "$URL"

# 3. put it into the system
install -m 0755 "$TMP/$BIN" /usr/local/bin/pve-real-memory
say "installed /usr/local/bin/pve-real-memory ($(/usr/local/bin/pve-real-memory version))"

# 4. self-install: config, monitoring key, vmstatus patch, systemd unit,
#    apt hook; pre-existing proxmox-rmem installs are torn down automatically
/usr/local/bin/pve-real-memory --install

say "done."
say "config: /etc/pve-real-memory/config.yaml (live-reloaded)"
say "status: pve-real-memory status; logs: journalctl -u pve-real-memory -f"
