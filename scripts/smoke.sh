#!/bin/sh
# Checks the minibox binary from outside, in the Lima VM: built on the Mac
# for Linux, run as root and as an ordinary user, and on the Mac itself.
set -u

here=$(cd "$(dirname "$0")/.." && pwd)
dev=${MINIBOX_DEV:-$HOME/.cache/minibox-dev}
vm=${MINIBOX_VM:-minibox}
bin=/mnt/minibox-dev/bin/minibox
passed=0
failed=0

check() { # name, then a command that must succeed
  name=$1; shift
  if "$@" >/dev/null 2>&1; then passed=$((passed + 1)); echo "ok   $name"
  else failed=$((failed + 1)); echo "FAIL $name"; fi
}

in_vm() { limactl shell "$vm" "$@"; }

mkdir -p "$dev/bin"
(cd "$here" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$dev/bin/minibox" ./cmd/minibox) || exit 1

check "a static Linux binary" sh -c "file '$dev/bin/minibox' | grep -q 'statically linked'"
check "check passes as root in the VM" in_vm sudo "$bin" check
check "every check is ok as root (no warnings)" sh -c "! limactl shell '$vm' sudo '$bin' check | grep -v '^ok '"
check "check fails without root" sh -c "limactl shell '$vm' '$bin' check; test \$? -eq 1"
check "and says why" sh -c "limactl shell '$vm' '$bin' check | grep -q '^FAIL root: running as uid'"
check "on the Mac, check fails and says Linux is needed" sh -c "cd '$here' && go run ./cmd/minibox check | grep -q 'needs Linux'"
check "an unknown command is a usage error (2)" sh -c "limactl shell '$vm' '$bin' bogus; test \$? -eq 2"
check "version" sh -c "limactl shell '$vm' '$bin' version | grep -q '^minibox '"

# Containers (Milestone 1), on the pinned root filesystem unpacked in the VM.
rootfs=$("$here/scripts/rootfs.sh" aarch64) || exit 1
in_vm sudo sh -c "rm -rf /var/tmp/minibox-smoke && mkdir -p /var/tmp/minibox-smoke && tar -xzf /mnt/minibox-dev/rootfs/$(basename "$rootfs") -C /var/tmp/minibox-smoke"
run="sudo $bin run --rootfs /var/tmp/minibox-smoke"
check "a container runs, with its own hostname" sh -c "test \"\$(limactl shell '$vm' $run --hostname smoke -- hostname)\" = smoke"
check "its init is PID 1 and sees no host process" sh -c "limactl shell '$vm' $run -- ps -o pid,args | awk 'NR>1' | grep -qv -e minibox-init -e 'ps -o' && exit 1; limactl shell '$vm' $run -- ps -o pid,args | grep -q '^ *1 minibox-init'"
check "the command's exit code comes back" sh -c "limactl shell '$vm' $run -- sh -c 'exit 7'; test \$? -eq 7"
check "a missing command is 127" sh -c "limactl shell '$vm' $run -- no-such-command 2>/dev/null; test \$? -eq 127"
check "run without root is 125" sh -c "limactl shell '$vm' '$bin' run --rootfs /var/tmp/minibox-smoke -- true 2>/dev/null; test \$? -eq 125"
check "nothing is left running" sh -c "! limactl shell '$vm' pgrep -f minibox-init"
check "no mount is left on the host" sh -c "! limactl shell '$vm' grep -q minibox-smoke /proc/self/mountinfo"
in_vm sudo rm -rf /var/tmp/minibox-smoke

# Images (Milestone 2): a real pull from Docker Hub, into a store of its own.
store="sudo env MINIBOX_ROOT=/var/tmp/minibox-smoke-store $bin"
in_vm sudo rm -rf /var/tmp/minibox-smoke-store
check "pull alpine:3.22 from Docker Hub" in_vm $store pull alpine:3.22
check "images lists it" sh -c "limactl shell '$vm' $store images | grep -q '^alpine:3.22 '"
check "run it: Alpine 3.22 inside" sh -c "limactl shell '$vm' $store run alpine:3.22 cat /etc/alpine-release | grep -q '^3\.22\.'"
check "a write stays in its container" sh -c "limactl shell '$vm' $store run alpine:3.22 sh -c 'echo x > /etc/written'; limactl shell '$vm' $store run alpine:3.22 test ! -e /etc/written"
check "no container folder is left" sh -c "test -z \"\$(limactl shell '$vm' sudo ls /var/tmp/minibox-smoke-store/containers)\""
check "no overlay is left mounted" sh -c "! limactl shell '$vm' grep -q minibox-smoke-store /proc/self/mountinfo"
# Limits (Milestone 3), on the same image.
check "--memory 32m stops a 64 MiB buffer, and says so" sh -c "limactl shell '$vm' $store run --memory 32m alpine:3.22 dd if=/dev/zero of=/dev/null bs=64M count=1 2>&1 | grep -q 'went over its memory limit'"
check "--pids 32 contains a fork bomb" sh -c "limactl shell '$vm' $store run --pids 32 alpine:3.22 sh -c 'f() { f | f & }; f; sleep 2; echo survived' 2>/dev/null | grep -q survived"
check "the container reads its own limits" sh -c "test \"\$(limactl shell '$vm' $store run --memory 32m alpine:3.22 cat /sys/fs/cgroup/memory.max)\" = 33554432"
check "no container cgroup is left" sh -c "test -z \"\$(limactl shell '$vm' sh -c 'ls -d /sys/fs/cgroup/minibox/*/ 2>/dev/null')\""
# Networking (Milestone 4), on the same image and a real web server.
check "a container has its own address on the bridge" sh -c "limactl shell '$vm' $store run alpine:3.22 ip -4 -o addr show eth0 | grep -q ' 10\.99\.0\.'"
check "it reaches the internet, by name (DNS and NAT)" in_vm $store run alpine:3.22 wget -q -T 10 -O /dev/null https://dl-cdn.alpinelinux.org/alpine/
check "--network none has only loopback" sh -c "test \"\$(limactl shell '$vm' $store run --network none alpine:3.22 sh -c 'ip -o link | wc -l')\" = 1"
check "pull nginx:1.29-alpine" in_vm $store pull nginx:1.29-alpine
in_vm sh -c "$store run -p 18080:80 nginx:1.29-alpine >/tmp/minibox-nginx.log 2>&1 &"
# "[r]un": a pattern that does not match the pkill or pgrep looking for it.
check "nginx answers on a published port" sh -c "limactl shell '$vm' sh -c 'for i in \$(seq 50); do curl -s -m 2 http://127.0.0.1:18080/ | grep -q \"Welcome to nginx\" && exit 0; sleep 0.2; done; exit 1'"
check "Ctrl-C (SIGINT) stops it" sh -c "limactl shell '$vm' sudo pkill -INT -f '[r]un -p 18080:80 nginx' && limactl shell '$vm' sh -c 'for i in \$(seq 50); do pgrep -f \"[r]un -p 18080:80\" >/dev/null || exit 0; sleep 0.2; done; exit 1'"
check "and the port is closed again, its rule gone" sh -c "! limactl shell '$vm' curl -s -m 2 http://127.0.0.1:18080/ && ! limactl shell '$vm' sudo nft list map ip minibox ports | grep -q 18080"
check "no veth or lease is left" sh -c "! limactl shell '$vm' sh -c 'ip -o link | grep -q mbx' && limactl shell '$vm' sudo cat /run/minibox/leases.json | grep -qx '{}'"
check "rmi nginx" in_vm $store rmi nginx:1.29-alpine
# Security (Milestone 5): capabilities, no_new_privs, seccomp, masks, userns.
check "no CAP_SYS_ADMIN: the container cannot mount" sh -c "! limactl shell '$vm' $store run alpine:3.22 sh -c 'mkdir -p /dev/shm/m && mount -t tmpfs none /dev/shm/m'"
check "no CAP_NET_RAW: ping has no raw socket" sh -c "! limactl shell '$vm' $store run alpine:3.22 ping -c 1 -W 1 10.99.0.1"
check "no_new_privs is set" sh -c "limactl shell '$vm' $store run alpine:3.22 grep -Eq 'NoNewPrivs:[[:space:]]*1' /proc/self/status"
check "a seccomp filter is installed" sh -c "limactl shell '$vm' $store run alpine:3.22 grep -Eq 'Seccomp:[[:space:]]*2' /proc/self/status"
check "/proc/kcore is masked (reads empty)" sh -c "limactl shell '$vm' $store run alpine:3.22 sh -c 'test ! -s /proc/kcore'"
check "/proc/sysrq-trigger is read-only" sh -c "! limactl shell '$vm' $store run alpine:3.22 sh -c 'echo 1 > /proc/sysrq-trigger'"
# --userns uses --rootfs: an image's store and layers are the host root's,
# which the mapped container root cannot read, so that is refused (below).
check "--userns with an image is refused (use --rootfs)" sh -c "limactl shell '$vm' $store run --userns alpine:3.22 true 2>&1 | grep -q 'not supported with an image'"
in_vm sudo sh -c "rm -rf /var/tmp/minibox-uns && mkdir -p /var/tmp/minibox-uns && tar -xzf /mnt/minibox-dev/rootfs/$(basename "$rootfs") -C /var/tmp/minibox-uns"
uns="sudo /mnt/minibox-dev/bin/minibox run --userns --network none --rootfs /var/tmp/minibox-uns"
check "--userns: the container's root is uid 0 inside" sh -c "test \"\$(limactl shell '$vm' $uns -- id -u)\" = 0"
check "--userns: it maps to an unprivileged host range" sh -c "limactl shell '$vm' $uns -- cat /proc/self/uid_map | grep -q 100000"
in_vm sh -c "$uns -- sleep 30 >/dev/null 2>&1 &"
check "--userns: the container process runs as that host uid" sh -c "limactl shell '$vm' sh -c 'for i in \$(seq 50); do ps -o uid= -C sleep 2>/dev/null | grep -qw 100000 && exit 0; sleep 0.1; done; exit 1'"
limactl shell "$vm" sudo pkill -f "run --userns" >/dev/null 2>&1
in_vm sudo rm -rf /var/tmp/minibox-uns
check "rmi removes the image and its layers" sh -c "limactl shell '$vm' $store rmi alpine:3.22 && test -z \"\$(limactl shell '$vm' sudo ls /var/tmp/minibox-smoke-store/layers)\""
check "an image that is not here is 125" sh -c "limactl shell '$vm' $store run alpine:3.22 true 2>/dev/null; test \$? -eq 125"
in_vm sudo rm -rf /var/tmp/minibox-smoke-store

echo "$passed passed, $failed failed"
[ "$failed" -eq 0 ]
