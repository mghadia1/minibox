#!/bin/sh
# Mac-side driver for the OCI comparison and benchmark (dev-only, runc as
# a reference). Builds the Linux binary, unpacks a root filesystem in the
# VM, and runs the given VM-side script there against runc:
#
#   scripts/vm-oci.sh scripts/oci_compare.sh
#   N=30 scripts/vm-oci.sh scripts/oci_startup.sh
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
dev=${MINIBOX_DEV:-$HOME/.cache/minibox-dev}
vm=${MINIBOX_VM:-minibox}
script=${1:?usage: vm-oci.sh SCRIPT (scripts/oci_compare.sh or scripts/oci_startup.sh)}

rootfs=$("$here/scripts/rootfs.sh" aarch64)
mkdir -p "$dev/bin"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$dev/bin/minibox" "$here/cmd/minibox"
cp "$script" "$dev/"
base=$(basename "$script")

limactl shell "$vm" sudo sh -c "
  command -v runc >/dev/null || { echo 'runc is not in the VM (the provisioning script installs it; or apt-get install runc)'; exit 1; }
  rm -rf /var/tmp/mb-oci-rootfs && mkdir -p /var/tmp/mb-oci-rootfs
  tar -xzf /mnt/minibox-dev/rootfs/$(basename "$rootfs") -C /var/tmp/mb-oci-rootfs
  MINIBOX=/mnt/minibox-dev/bin/minibox ROOTFS=/var/tmp/mb-oci-rootfs ${N:+N=$N} sh /mnt/minibox-dev/$base
"
