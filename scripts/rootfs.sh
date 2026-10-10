#!/bin/sh
# Downloads the root filesystem the tests run containers in (Alpine's
# minirootfs, 4 MB), checked against its pinned SHA-256, into the
# development cache, outside the repository. Prints the file's path.
#
#   scripts/rootfs.sh [aarch64|x86_64]    (default: this machine's)
set -eu

version=3.22.6
arch=${1:-$(uname -m)}
case "$arch" in
  arm64 | aarch64) arch=aarch64; sum=821565fa8f3953eefd12497b166b4b50add2f7c57fb312e75862f5867e06fefe ;;
  x86_64 | amd64) arch=x86_64; sum=27694aaa55fd7a9e3ef596e0ad4eb66802308bb20172b17030cd5f4d8ae9bac2 ;;
  *) echo "rootfs: no pinned rootfs for $arch" >&2; exit 2 ;;
esac

dir=${MINIBOX_DEV:-$HOME/.cache/minibox-dev}/rootfs
file=$dir/alpine-minirootfs-$version-$arch.tar.gz
url=https://dl-cdn.alpinelinux.org/alpine/v${version%.*}/releases/$arch/alpine-minirootfs-$version-$arch.tar.gz

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

mkdir -p "$dir"
if [ ! -f "$file" ] || [ "$(sha256 "$file")" != "$sum" ]; then
  curl -fsSL -o "$file.part" "$url"
  got=$(sha256 "$file.part")
  if [ "$got" != "$sum" ]; then
    rm -f "$file.part"
    echo "rootfs: $url has SHA-256 $got, expected $sum" >&2
    exit 1
  fi
  mv "$file.part" "$file"
fi
echo "$file"
