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
check "rmi removes the image and its layers" sh -c "limactl shell '$vm' $store rmi alpine:3.22 && test -z \"\$(limactl shell '$vm' sudo ls /var/tmp/minibox-smoke-store/layers)\""
check "an image that is not here is 125" sh -c "limactl shell '$vm' $store run alpine:3.22 true 2>/dev/null; test \$? -eq 125"
in_vm sudo rm -rf /var/tmp/minibox-smoke-store

echo "$passed passed, $failed failed"
[ "$failed" -eq 0 ]
