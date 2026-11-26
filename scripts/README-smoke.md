# trixie-smoke.sh

Full lifecycle verification against a real Proxmox QE 9 (Debian 13)
stack, no host required:

    make build
    docker run --rm --platform linux/amd64 \
      -v $PWD/bin:/mount/bin:ro -v $PWD/scripts/trixie-smoke.sh:/smoke.sh:ro \
      debian:trixie bash /smoke.sh

Inside debian:trixie it installs the real `qemu-server` package from
the pve-no-subscription trixie repo (PVE 9: package renamed from
pve-qemu-server), stubs systemctl, then drives the binary through
--install, doctor, once, a functional run of the embedded Perl hook
(fresh/stale/garbage override), an apt reinstall that triggers the
Post-Invoke self-repair, and --uninstall with a byte-exact sha256
comparison of QemuServer.pm. Ends with ALL-TRIXIE-CHECKS-PASSED.
