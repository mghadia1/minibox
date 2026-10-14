#!/bin/sh
# Measures OCI start-up time -- `create` then `start` -- for MiniBox and
# for runc on the same bundle, as a rough side-by-side. Run inside the
# Linux VM as root:
#
#   MINIBOX=/path/to/minibox ROOTFS=/path/to/alpine bench/oci_startup.sh
#
# Caveats, always quoted with any number: one 8 GB M2 laptop, one 2 GiB
# Lima VM; the container command is `true`, so this times the runtime's
# own work, not a workload; runc applies the whole config (all mounts,
# capabilities, a libseccomp filter) while MiniBox applies its subset and
# its own fixed hardening, so this is an order-of-magnitude comparison,
# not a controlled one. The swap counters are printed: if they moved, the
# run was under memory pressure and the numbers are noise.
set -u

minibox=${MINIBOX:?set MINIBOX}
rootfs=${ROOTFS:?set ROOTFS}
runc=${RUNC:-runc}
n=${N:-30}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

bundle="$work/b"
mkdir -p "$bundle"
(cd "$bundle" && "$runc" spec >/dev/null 2>&1)
python3 - "$bundle/config.json" "$rootfs" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
cfg["root"] = {"path": sys.argv[2], "readonly": False}
cfg["process"]["terminal"] = False
cfg["process"]["args"] = ["true"]
json.dump(cfg, open(sys.argv[1], "w"))
PY

now() { date +%s%N; }
swapin() { awk '/^pswpin/{print $2}' /proc/vmstat; }

# bench RUNTIME LABEL: time create and start n times, print the medians.
bench() {
	rt=$1 label=$2
	creates="$work/creates" starts="$work/starts"
	: >"$creates"
	: >"$starts"
	# warm up
	for _ in 1 2 3; do "$rt" create -b "$bundle" w$$ >/dev/null 2>&1; "$rt" start w$$ >/dev/null 2>&1; "$rt" delete --force w$$ >/dev/null 2>&1; done
	i=0
	while [ "$i" -lt "$n" ]; do
		id="b$$-$i"
		t0=$(now); "$rt" create -b "$bundle" "$id" >/dev/null 2>&1; t1=$(now)
		"$rt" start "$id" >/dev/null 2>&1; t2=$(now)
		"$rt" delete --force "$id" >/dev/null 2>&1
		echo $(( (t1 - t0) / 1000000 )) >>"$creates"
		echo $(( (t2 - t1) / 1000000 )) >>"$starts"
		i=$((i + 1))
	done
	cm=$(sort -n "$creates" | awk '{a[NR]=$1} END{print a[int((NR+1)/2)]}')
	sm=$(sort -n "$starts" | awk '{a[NR]=$1} END{print a[int((NR+1)/2)]}')
	printf '%-10s create %4s ms   start %4s ms   total %4s ms  (median of %d)\n' "$label" "$cm" "$sm" "$((cm + sm))" "$n"
}

echo "OCI start-up, median of $n, one 2 GiB VM on one laptop; command: true"
before=$(swapin)
bench "$minibox" minibox
bench "$runc" runc
after=$(swapin)
echo "swap-ins during the run: $((after - before)) (non-zero means the numbers are noise)"
