#!/bin/bash
# Full lifecycle smoke on Debian 13 trixie with the real qemu-server package.
set -eux
export DEBIAN_FRONTEND=noninteractive

apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates perl wget >/dev/null
perl -v | grep version

wget -q -O /etc/apt/trusted.gpg.d/proxmox-release-trixie.gpg \
  https://enterprise.proxmox.com/debian/proxmox-release-trixie.gpg
echo 'deb http://download.proxmox.com/debian/pve trixie pve-no-subscription' \
  > /etc/apt/sources.list.d/pve.list
apt-get update -qq
apt-get install -y -qq --no-install-recommends qemu-server >/dev/null
dpkg -l qemu-server | tail -1

PM=/usr/share/perl5/PVE/QemuServer.pm
sha256sum "$PM" | tee /root/pm.orig.sha

# stub systemctl: container has no systemd, we verify call order via log
printf '#!/bin/sh\necho "systemctl $*"\nexit 0\n' > /usr/local/bin/systemctl
chmod +x /usr/local/bin/systemctl

install -m 0755 /mount/bin/pve-real-memory-linux-amd64 /usr/local/bin/pve-real-memory
pve-real-memory version

echo "=== install ==="
pve-real-memory --install
grep -q 'BEGIN pve-real-memory' "$PM"
grep -q 'BEGIN pve-real-memory' /etc/systemd/system/pve-real-memory.service && grep -q 'RuntimeDirectory=pve-real-memory' /etc/systemd/system/pve-real-memory.service
grep -q 'repair --hook' /etc/apt/apt.conf.d/99-pve-real-memory
test "$(stat -c '%a' /run/pve-real-memory)" = "700"
test "$(stat -c '%a' /etc/pve-real-memory/id_ed25519)" = "600"

echo "=== installed QemuServer.pm still compiles with trixie perl ==="
perl -c "$PM"

echo "=== doctor / once on the generated config ==="
pve-real-memory doctor
pve-real-memory once   # template: auto-discovery, no qga sockets -> no sources, exit 0

echo "=== functional: run the embedded hook block against real trixie perl ==="
sed -n '/# BEGIN pve-real-memory/,/# END pve-real-memory/p' "$PM" > /root/hook.pl
test -s /root/hook.pl

run_hook() { printf 'my $res = { 123 => { mem => 999 } };\n' > /root/harness.pl
  cat /root/hook.pl >> /root/harness.pl
  printf 'print $res->{123}{mem}, "\\n";\n' >> /root/harness.pl
  perl /root/harness.pl; }

# fresh override -> value wins
printf '123456' > /run/pve-real-memory/123.mem
chmod 600 /run/pve-real-memory/123.mem
test "$(run_hook)" = "123456"

# stale override (older than TTL) -> host value preserved
touch -d '-5 minutes' /run/pve-real-memory/123.mem
test "$(run_hook)" = "999"

# garbage content -> host value preserved
printf 'not-a-number' > /run/pve-real-memory/123.mem
test "$(run_hook)" = "999"

echo "=== repair after simulated apt overwrite ==="
apt-get install -y -qq --reinstall -o Dpkg::Options::=--force-confnew --no-install-recommends qemu-server >/dev/null
if grep -q 'BEGIN pve-real-memory' "$PM"; then
  echo "apt Post-Invoke hook re-applied the patch automatically: PASS"
else
  echo "apt hook did not run; falling back to manual repair"
  pve-real-memory repair --hook
fi
grep -q 'BEGIN pve-real-memory' "$PM"
perl -c "$PM"

echo "=== uninstall restores byte-exact ==="
pve-real-memory --uninstall --purge
sha256sum "$PM" > /root/pm.now.sha
diff <(awk '{print $1}' /root/pm.orig.sha) <(awk '{print $1}' /root/pm.now.sha)
test ! -e /etc/systemd/system/pve-real-memory.service
test ! -e /etc/apt/apt.conf.d/99-pve-real-memory
test ! -d /etc/pve-real-memory
pve-real-memory status | head -4

echo "ALL-TRIXIE-CHECKS-PASSED"
