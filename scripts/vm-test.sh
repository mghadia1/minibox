#!/bin/sh
# Runs the checks in the Lima VM, as root: the source is copied into the
# development cache the VM mounts (never the iCloud-synced project folder),
# then gofmt, go vet and go test -race run there. Arguments go to go test.
#
#   scripts/vm-test.sh [-run Pattern] [-v]
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
dev=${MINIBOX_DEV:-$HOME/.cache/minibox-dev}
vm=${MINIBOX_VM:-minibox}

rootfs=$("$here/scripts/rootfs.sh" aarch64)
mkdir -p "$dev/src"
rsync -a --delete --exclude .git --exclude bin "$here/" "$dev/src/"

# The VM sees the cache at /mnt/minibox-dev (lima/minibox.yaml).
limactl shell --workdir /mnt/minibox-dev/src "$vm" sudo \
  env MINIBOX_TEST_REQUIRE=1 MINIBOX_TEST_ROOTFS="/mnt/minibox-dev/rootfs/$(basename "$rootfs")" GOFLAGS=-buildvcs=false \
  sh -c '
    set -e
    unformatted=$(gofmt -l .)
    if [ -n "$unformatted" ]; then echo "not gofmt-clean:"; echo "$unformatted"; exit 1; fi
    go vet ./...
    go test -race -count=1 -timeout 5m "$@" ./...
  ' sh "$@"
