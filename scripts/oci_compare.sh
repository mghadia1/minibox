#!/bin/sh
# Compares MiniBox against runc on the same OCI bundles: for each command,
# both runtimes create / start / wait / delete a container, and their
# output and lifecycle are compared. runc is a reference only (never the
# engine). Run inside the Linux VM as root:
#
#   MINIBOX=/path/to/minibox ROOTFS=/path/to/alpine scripts/oci_compare.sh
#
# A bundle's config.json is `runc spec`'s default, with terminal turned
# off and the process/cwd set per case. MiniBox reads the subset it
# supports and ignores the rest (capabilities, mounts, seccomp): the
# comparison is of lifecycle and process behaviour, not of those.
set -u

minibox=${MINIBOX:?set MINIBOX to the minibox binary}
rootfs=${ROOTFS:?set ROOTFS to an unpacked root filesystem}
runc=${RUNC:-runc}
work=$(mktemp -d)
pass=0
fail=0

cleanup() {
  "$minibox" delete --force cmp-mb >/dev/null 2>&1
  "$runc" delete --force cmp-runc >/dev/null 2>&1
  rm -rf "$work"
}
trap cleanup EXIT

# run RUNTIME ID BUNDLE OUTFILE: create (output to OUTFILE), start, wait
# for stopped, delete. Leaves the status path's final status in $status.
run_container() {
  rt=$1 id=$2 bundle=$3 out=$4
  if [ "$rt" = "$minibox" ]; then
    "$rt" create -b "$bundle" "$id" >/dev/null 2>&1 || return 1
    "$rt" start "$id" >/dev/null 2>&1 || return 1
    for _ in $(seq 100); do
      [ "$("$rt" state "$id" 2>/dev/null | sed -n 's/.*"status": *"\([a-z]*\)".*/\1/p')" = stopped ] && break
      sleep 0.05
    done
    cp "/run/minibox/oci/$id/output.log" "$out" 2>/dev/null
    "$rt" delete "$id" >/dev/null 2>&1
  else
    # runc: the container inherits create's stdio; redirect it to $out.
    "$rt" create -b "$bundle" "$id" >"$out" 2>&1 </dev/null || return 1
    "$rt" start "$id" >/dev/null 2>&1 || return 1
    for _ in $(seq 100); do
      [ "$("$rt" state "$id" 2>/dev/null | sed -n 's/.*"status": *"\([a-z]*\)".*/\1/p')" = stopped ] && break
      sleep 0.05
    done
    "$rt" delete "$id" >/dev/null 2>&1
  fi
}

compare() { # name  cwd  arg...
  name=$1 cwd=$2
  shift 2
  bundle="$work/b"
  rm -rf "$bundle"
  mkdir -p "$bundle"
  (cd "$bundle" && "$runc" spec >/dev/null 2>&1) || { echo "FAIL $name (runc spec)"; fail=$((fail + 1)); return; }
  # root.path is the absolute root filesystem (both runtimes refuse a
  # symlink); terminal off; the process is this case's command.
  python3 - "$bundle/config.json" "$rootfs" "$cwd" "$@" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
cfg["root"] = {"path": sys.argv[2], "readonly": False}
cfg["process"]["terminal"] = False
cfg["process"]["cwd"] = sys.argv[3]
cfg["process"]["args"] = sys.argv[4:]
json.dump(cfg, open(sys.argv[1], "w"))
PY
  rm -f "$work/mb.out" "$work/runc.out"
  run_container "$minibox" cmp-mb "$bundle" "$work/mb.out"
  mbrc=$?
  run_container "$runc" cmp-runc "$bundle" "$work/runc.out"
  rcrc=$?
  if [ "$mbrc" != "$rcrc" ]; then
    echo "FAIL $name: minibox rc=$mbrc runc rc=$rcrc"; fail=$((fail + 1)); return
  fi
  if ! diff -q "$work/mb.out" "$work/runc.out" >/dev/null 2>&1; then
    echo "FAIL $name: output differs"
    echo "  minibox: $(tr '\n' '|' <"$work/mb.out")"
    echo "  runc:    $(tr '\n' '|' <"$work/runc.out")"
    fail=$((fail + 1)); return
  fi
  echo "ok   $name  ->  $(tr '\n' '|' <"$work/mb.out")"
  pass=$((pass + 1))
}

# Commands are bare names, found on PATH by both runtimes, so the test
# does not depend on where BusyBox links each applet.
compare "echo"            /    echo hello world
compare "id -u (root)"    /    id -u
compare "hostname"        /    hostname
compare "cwd"             /tmp pwd
compare "env PATH"        /    sh -c 'echo $PATH'
compare "a written file"  /    sh -c 'echo x > /tmp/f; cat /tmp/f'
compare "exit status 0"   /    true
compare "stdout + stderr" /    sh -c 'echo out; echo err 1>&2'

echo
echo "$pass matched, $fail differed"
[ "$fail" -eq 0 ]
