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

echo "$passed passed, $failed failed"
[ "$failed" -eq 0 ]
