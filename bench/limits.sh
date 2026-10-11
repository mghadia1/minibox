#!/bin/sh
# Measures what the limits do, in the Lima VM: how much CPU a container
# that spins for SECONDS of wall time gets under each --cpus, and where
# memory limits stop an allocation. Run on an idle VM (it checks the load
# first). Results go to bench/results/limits-TIME.txt.
#
#   bench/limits.sh [SECONDS] [RUNS]
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
dev=${MINIBOX_DEV:-$HOME/.cache/minibox-dev}
vm=${MINIBOX_VM:-minibox}
seconds=${1:-5}
runs=${2:-3}
out="$here/bench/results/limits-$(date -u +%Y%m%dT%H%M%SZ).txt"
mkdir -p "$here/bench/results"

(cd "$here" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$dev/bin/minibox" ./cmd/minibox \
  && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$dev/bin/probe" ./internal/testutil/probe)
rootfs=$("$here/scripts/rootfs.sh" aarch64)

limactl shell "$vm" sudo sh -s "$seconds" "$runs" "$(basename "$rootfs")" > "$out" <<'SCRIPT'
set -eu
seconds=$1; runs=$2; archive=$3
r=/var/tmp/minibox-bench
rm -rf $r && mkdir -p $r && tar -xzf /mnt/minibox-dev/rootfs/$archive -C $r && cp /mnt/minibox-dev/bin/probe $r/probe
m=/mnt/minibox-dev/bin/minibox
echo "# $(date -u +%FT%TZ) $(uname -r), $(nproc) CPUs in the VM, load at start: $(cut -d' ' -f1-3 /proc/loadavg)"
echo "# each run: /probe spin $seconds (one thread busy for $seconds s of wall time)"
echo "cpus,run,cpu_seconds,throttled_seconds,expected_cpu_seconds"
for cpus in 0.25 0.5 1 0; do
  for run in $(seq "$runs"); do
    flag=""; [ "$cpus" != 0 ] && flag="--cpus $cpus"
    line=$($m run --stats $flag --rootfs $r -- /probe spin "$seconds" 2>&1 >/dev/null | grep 'minibox: cpu')
    cpu=$(echo "$line" | sed -E 's/.*cpu ([0-9.]+) s.*/\1/')
    held=$(echo "$line" | sed -E 's/.*held back ([0-9.]+) s.*/\1/')
    want=$(awk -v c="$cpus" -v s="$seconds" 'BEGIN { print (c == 0 ? s : c * s) }')
    echo "$cpus,$run,$cpu,$held,$want"
  done
done
echo "# memory: /probe alloc MIB under --memory 64m"
echo "alloc_mib,exit,oom_kills,peak"
for mib in 16 48 60 80 200; do
  set +e
  $m run --stats --memory 64m --rootfs $r -- /probe alloc "$mib" >/dev/null 2>/tmp/minibox-bench.err
  code=$?
  set -e
  line=$(grep 'minibox: cpu' /tmp/minibox-bench.err)
  oom=$(echo "$line" | sed -E 's/.*killed for memory ([0-9]+).*/\1/')
  peak=$(echo "$line" | sed -E 's/.*memory peak ([0-9.]+ [KMG]?i?B).*/\1/')
  echo "$mib,$code,$oom,$peak"
done
rm -rf $r
SCRIPT
cat "$out"
echo "results: $out"
