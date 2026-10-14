# MiniBox

A container runtime built from the Linux kernel's own pieces: namespaces,
`pivot_root`, cgroup v2, overlayfs, virtual network devices, capabilities
and seccomp. The point is to know what Docker and runc do underneath, so
none of runc, containerd, libcontainer, go-containerregistry, CNI plugins,
netlink libraries or libseccomp is used; `golang.org/x/sys/unix` (system
call wrappers) is allowed, and runc may appear in tests as a reference.

> **Status:** Milestones 0-6 of 7 built, on my machine (the GitHub
> repository is empty, so CI has not run). `minibox pull alpine:3.22`
> fetches an image from its registry, checking every digest, and unpacks
> its layers safely; `minibox run alpine:3.22 sh` runs it on an overlay of
> those layers, with its own hostname, process ids, mounts, IPC, cgroup and
> network, under an init that reaps and passes signals on, within memory,
> CPU and process limits (`--memory`, `--cpus`, `--pids`). Each container
> has its own address on a bridge, reaches the internet through the host,
> and can publish ports (`-p 8080:80`). The command runs with a small
> capability set, `no_new_privs`, a hand-written seccomp filter, and masked
> and read-only `/proc` and `/sys` paths; `--userns` gives it its own user
> namespace, so its root is an unprivileged user on the host. It also
> speaks a subset of the OCI runtime lifecycle — `minibox create` /
> `start` / `state` / `kill` / `delete` on a bundle, as runc does. **It is
> still not something to trust with hostile code:** it has no devices
> cgroup, one shared bridge, and the user namespace does not id-map the
> root filesystem (see Security).

> **Where it runs:** Linux only, as root. On my Mac that is a Lima VM
> (`lima/minibox.yaml`: Ubuntu 24.04, 2 CPUs, 2 GiB, 10 GiB disk); in CI,
> GitHub's Ubuntu runners. MiniBox changes the network setup of the
> machine it runs on (a bridge, nftables rules, IP forwarding turned on),
> so it belongs in a disposable VM or CI runner, not on a machine anyone
> depends on.

## Status

| # | Milestone | Status |
|---|-----------|--------|
| 0 | Skeleton: Go module, `minibox` CLI, the Lima VM, tests in the VM as root, CI, a pinned test root filesystem | Built |
| 1 | Isolation: namespaces, `pivot_root`, `/proc` and `/dev`, an init as PID 1 | Built |
| 2 | Images: pulled from a registry and checked, safe layer extraction, overlayfs | Built |
| 3 | Limits: cgroup v2 memory, CPU and pids; OOM kills reported | Built |
| 4 | Networking: veth pairs on a bridge, NAT and published ports, netlink by hand | Built |
| 5 | Security: capabilities cut, `no_new_privs`, a hand-written seccomp filter, masked paths, an optional user namespace | Built |
| 6 | The OCI runtime lifecycle (a subset: create/start/state/kill/delete), compared with runc; start-up time measured | Built |
| 7 | README: diagram, decisions, failures, results; explain-it-yourself prompts | Not started |

"Built" means built and tested on my machine, in the VM.

## Quickstart

On a Mac with Homebrew and Go 1.27:

```bash
brew install lima
make vm          # once: a 620 MB Ubuntu image, then Go and gcc inside it
make vm-test     # gofmt, go vet, go test -race, in the VM, as root
make smoke       # the binary from outside: check and run in the VM, as root and not, and on the Mac
```

On Linux, as root:

```bash
go build -o bin/minibox ./cmd/minibox
sudo ./bin/minibox check
```

`minibox check` says, one line each, whether the machine has what MiniBox
needs: root, the namespaces (uts, pid, mnt, ipc, net, user, cgroup),
cgroup v2 with the cpu, memory and pids controllers, overlayfs, and two
warnings rather than failures: nftables (without it only `--network none`
and `host` work) and user namespaces (for Milestone 5). Exit 0 when
everything needed is there, 1 when something is missing, 2 for a usage
error.

## Running a container

In the VM, as root:

```bash
minibox pull alpine:3.22
minibox run alpine:3.22 cat /etc/alpine-release
minibox run -u nobody -w /tmp alpine:3.22 sh -c 'id; pwd'
minibox images
minibox rmi alpine:3.22
```

Or on a root filesystem unpacked into a directory, without an image:

```bash
mkdir /var/tmp/alpine
tar -xzf /mnt/minibox-dev/rootfs/alpine-minirootfs-3.22.6-aarch64.tar.gz -C /var/tmp/alpine
minibox run --rootfs /var/tmp/alpine --hostname box -- /bin/sh -c 'hostname; ps'
```

```
box
PID   USER     TIME  COMMAND
    1 root      0:00 minibox-init init
    8 root      0:00 /bin/sh -c hostname; ps
   10 root      0:00 ps
```

Options: `--hostname` (default: the container's id, 12 hex digits), `-e
KEY=value` (repeatable), `-w DIR` (the working directory inside), `-u
USER` (a name, uid, `name:group` or `uid:gid`, looked up in the
container's own `/etc/passwd` and `/etc/group`), `--entrypoint` (replaces
the image's; `""` for none), `--keep` (keep the container's writable layer
after it exits), `--memory`/`--swap`/`--cpus`/`--pids` (limits),
`--network bridge|none|host` and `-p HOST:CONTAINER` (networking), and
`--userns` (a user namespace). The exit code is the command's, or 128 + the signal that
ended it; 125 when MiniBox could not run the container, 126 when the
command could not be invoked, 127 when it was not found (Docker's codes).
With `--rootfs`, the command writes straight into that directory; with an
image, into a layer of its own that is deleted when it exits.

## Images

Images and containers live in `/var/lib/minibox` (`$MINIBOX_ROOT`):

```
blobs/sha256/HEX      manifests and configs, by digest
layers/HEX            each layer unpacked, named by its diff_id, shared between images
images.json           "alpine:3.22" -> manifest, config, layers
containers/ID/        upper, work, merged: one container's overlay, deleted with it
```

### Pulling

```
minibox pull alpine:3.22 ──► registry-1.docker.io/library/alpine, tag 3.22 (Docker's naming rules)
   ──► GET /v2/library/alpine/manifests/3.22 ──► 401 with a Bearer challenge
       ──► an anonymous token from the realm it names ──► the request again
   ──► an index (one manifest per platform) ──► linux/arm64/v8 chosen ──► that manifest, by digest
   ──► the config (checked against its digest): os and architecture must be this machine's,
       one diff_id per layer
   ──► each layer streamed: registry ──► SHA-256 of what arrives ──► gunzip ──► SHA-256 of the tar
       ──► unpacked into layers/.tmp-RANDOM ──► both digests checked ──► renamed into place
   ──► manifest and config saved, images.json written (a temporary file, then a rename)
```

- **Every byte is checked against a digest before it is used:** a
  manifest against the digest it was asked for (or the registry's
  `Docker-Content-Digest`), the config and each compressed layer against
  their descriptors (size too), and each unpacked layer against the
  config's diff_id. A layer is renamed into place only when all of them
  match; a pull that fails leaves no layer, no temporary folder and no
  image.
- **A layer is unpacked while it downloads,** so nothing compressed is
  kept, and the unpacking must be safe for a layer that turns out to be
  bad (it is: see below). A layer already in the store is not fetched
  again.
- **Limits:** a manifest is at most 4 MB, a config 4 MB, a layer's files
  4 GiB in all (the VM's disk is 10 GiB). zstd layers are refused: Go's
  standard library has no zstd.
- **The only network MiniBox uses** is `minibox pull`, to the registry an
  image names, anonymously. No credentials are supported.

### Unpacking a layer safely

A layer is a tar file from somewhere else, so every entry is treated as
hostile (`internal/image/extract_linux.go`):

- **Nothing is written outside the layer's folder.** Every parent
  directory is opened with `openat2(RESOLVE_IN_ROOT)`, which resolves `..`
  and symbolic links as if the layer's folder were `/`; the entry itself
  is made with the `*at` system calls on that directory, never following a
  link in its place (`O_NOFOLLOW`, `AT_SYMLINK_NOFOLLOW`). A name with `..`
  is refused outright (Docker accepts `a/../b`; MiniBox is stricter). A
  file written over a symbolic link replaces the link. A hard link must
  point at something already in the layer, and a hard link to a symbolic
  link is to the link itself.
- **Whiteouts, as overlayfs reads them:** a `.wh.NAME` entry becomes a
  0/0 character device named NAME, and `.wh..wh..opq` marks its directory
  opaque (`trusted.overlay.opaque=y`). A layer's own `trusted.*` attributes
  are never applied (it could forge whiteouts with them); only file
  capabilities and `user.*` are.
- **As tar would:** owner, mode (after the owner: `chown` clears
  set-user-id bits), times (directories' last, since filling a directory
  changes its time), devices, FIFOs, missing parent directories made
  `0755`, and a later entry replacing an earlier one.

### Running an image

`minibox run IMAGE [COMMAND...]` makes `containers/ID/{upper,work,merged}`
and Init mounts the image's layers there as an overlay, top layer first,
inside the container's own mount namespace: the mount cannot outlive the
container, even if `minibox run` is killed. The image's configuration is
applied as Docker applies it: the entrypoint, then the command given (or
the image's `Cmd`); its `Env` (with `-e` on top); its `WorkingDir`, made
if missing; its `User` (or `-u`), looked up in the container's own
`/etc/passwd` and `/etc/group`, with the user's supplementary groups;
`HOME` from that passwd entry.

## Limits

```bash
minibox run --memory 256m --cpus 0.5 --pids 100 --stats alpine:3.22 sh
```

Every container gets a cgroup v2 group of its own,
`/sys/fs/cgroup/minibox/ID`, made before the container exists and removed
after it ends:

| Flag | cgroup file | Means |
|------|-------------|-------|
| `--memory 256m` | `memory.max`; `memory.swap.max` 0 | over the limit, the kernel kills the largest process in the group (no swapping instead, unless `--swap` allows it) |
| `--swap 512m` | `memory.swap.max` | swap on top of `--memory` |
| `--cpus 0.5` | `cpu.max` "50000 100000" | half a CPU's time in every 100 ms period, however many threads |
| `--pids 100` | `pids.max` | at most 100 processes and threads at once: `fork` fails with EAGAIN above it |

- **Born inside its group:** Init is started with clone3's
  `CLONE_INTO_CGROUP` (Go's `SysProcAttr.UseCgroupFD`), so no process of
  the container ever runs outside its limits, not even between starting
  and being moved.
- **Its own view:** the container gets a cgroup namespace, so its group
  is its `/` (`/proc/self/cgroup` reads `0::/`), mounted read-only at
  `/sys/fs/cgroup`: it can read its own limits and usage, not change them.
- **Afterwards:** what it used is read from the group (`memory.events`,
  `memory.peak`, `cpu.stat`, `pids.events`). If the kernel killed a process
  for memory, `minibox run` says so; `--stats` prints CPU time, time held
  back by the CPU limit, peak memory and forks refused. Then anything still
  in the group is killed (`cgroup.kill`) and the group removed. A group
  left by a `minibox run` that was killed outright is removed by a later
  one, once it is empty and over a minute old.
- **Where:** `minibox` enables the cpu, memory and pids controllers for
  its own groups (`cgroup.subtree_control`). It does not go through
  systemd: on a machine where systemd should own every group, Docker's
  systemd driver is the polite way, and this is not it.

### Measured (2026-10-10)

`bench/limits.sh` in the VM (Ubuntu 24.04, kernel 6.8, 2 CPUs), idle at
the start (load 0.23): a probe keeping one thread busy for 5 s of wall
time, three runs per setting (`bench/results/limits-20261010T082434Z.txt`):

| `--cpus` | CPU used | Expected | Held back by the limit |
|----------|----------|----------|------------------------|
| 0.25 | 1.28-1.29 s | 1.25 s | 3.85-3.89 s |
| 0.5 | 2.52-2.55 s | 2.5 s | 2.44-2.51 s |
| 1 | 4.99 s | 5 s | 0.01-0.02 s |
| none | 4.97-5.01 s | 5 s | 0 |

The quota holds to within 3% (a little over, never under). Under
`--memory 64m`, allocating and touching 16, 48 and 60 MiB ran (peak 23,
55 and 64 MiB: the kernel took back cached file pages to fit the 60), and
80 and 200 MiB were killed for memory (exit 137, one OOM kill each). One
VM on one laptop: these say the limits work as written, not how they
behave on a loaded server.

## Networking

```bash
minibox run -p 8080:80 nginx:1.29-alpine             # http://127.0.0.1:8080, or the host's address
minibox run alpine:3.22 wget -qO- http://10.99.0.2/  # another container, by its address
minibox run --network none alpine:3.22 ip link       # loopback only
minibox run --network host alpine:3.22 ip link       # the host's own network: no isolation
```

By default a container is on MiniBox's bridge, as on Docker's default
bridge network:

```
host:       minibox0, a bridge, 10.99.0.1/24 ──── mbxID (host end) ─┐ a veth pair
container:  eth0 10.99.0.N/24, default route via 10.99.0.1 ─────────┘ and lo, up
nftables, table ip minibox (MiniBox's own):
   prerouting, output   traffic to any of the host's addresses on a published port:
                        DNAT (tcp, 8080) -> (10.99.0.N, 80), from a map
   postrouting          masquerade: a container's traffic leaving the host; the host's own
                        127.0.0.1 reaching a container; a container reaching a published
                        port through the host's address (the answer must come back the same way)
   forward              to and from the bridge, accepted
```

- **Wired up from both sides.** `minibox run` starts Init in a new network
  namespace, and Init waits for its configuration. Meanwhile `minibox
  run`, still in the host's namespace, leases an address, makes the veth
  pair with one end created straight into Init's namespace as `eth0`
  (`IFLA_NET_NS_PID`), puts the host end on the bridge and sets it up, and
  adds the published ports. Then it sends the configuration, and Init,
  inside, sets up loopback, gives eth0 its address, sets it up, adds the
  default route, and writes `/etc/hosts` and `/etc/resolv.conf`.
- **Netlink by hand** (`internal/netlink`): no netlink library and no `ip`
  command. MiniBox builds each message itself: a 16-byte header, an
  `ifinfomsg`, `ifaddrmsg` or `rtmsg`, and attributes, each a 4-byte
  header and data padded to 4 bytes, nested for a veth's peer
  (`IFLA_LINKINFO` > `IFLA_INFO_DATA` > `VETH_INFO_PEER` > a whole
  `ifinfomsg` with attributes of its own). Every request asks for an
  acknowledgement, and a refusal comes back as its errno (`EEXIST`,
  `ENODEV`, ...).
- **nftables through the `nft` command,** not by hand: nftables' own
  netlink protocol is a bytecode for rules, far bigger than the few link,
  address and route messages above. MiniBox has one table of its own,
  written when it is missing; a published port is one element of a map,
  added when its container starts and deleted when it ends, so nothing
  else is rewritten. `route_localnet` on the bridge lets a connection to
  the host's own `127.0.0.1` be sent on to a container. Changing the rules
  means deleting the table while no container runs (`nft delete table ip
  minibox`): they are not rewritten over an existing table.
- **Addresses are leased** from `/run/minibox/leases.json` under a lock
  (`flock`), so containers started at once get different ones: the lowest
  free of .2-.254, recorded with the pid of the `minibox run` holding it
  and when that process started. A lease whose holder has died (killed
  outright, so it could not give it back) is taken back by the next
  container to start, with its published ports; its veth went already,
  with its namespace. The start time is there because pids are reused: a
  new process with a dead holder's number would otherwise keep the lease
  forever. `/run` is a tmpfs: leases do not outlive a reboot, and neither
  do containers.
- **DNS:** the host's resolvers, from systemd-resolved's list of upstream
  servers when it runs, without any on loopback (the host's `127.0.0.53`
  cannot be reached from a container's namespace).
- **`--network none`** gives a network namespace with only loopback;
  **`--network host`** no network namespace at all, and `-p` is refused
  there.
- **Ports:** `-p HOST:CONTAINER`, `-p PORT` (the same on both sides),
  `/tcp` (the default) or `/udp`, repeatable. A port already published by
  a running container is refused (125), and so is one a program on the
  host holds (a TCP listener or a bound UDP socket, read from
  `/proc/net/tcp`, `tcp6`, `udp` and `udp6`): DNAT happens before the
  kernel looks for a socket, so publishing the VM's port 22 would quietly
  take its SSH away. Docker's proxy would fail to bind the port instead. A
  program that starts listening on a published port later gets nothing.

**Not built:** IPv6, networks of your own, container names in DNS, port
ranges, publishing on one host address only, and anything about other
firewalls on the host. **With Docker on the same machine, traffic through
the host is dropped:** Docker sets the `FORWARD` policy to DROP, and a
packet must be accepted by every table, so MiniBox's own `accept` does not
save it. Tried in the VM by setting that policy: traffic out to another
machine, in through a published port, and from a container to a port
published on the host's address all timed out, and `iptables -P FORWARD
ACCEPT` brought them back. GitHub's runners have Docker, so CI sets that
policy first (written, not yet run). Docker also loads `br_netfilter`,
which sends traffic between containers on one bridge through the same
rules; that case was not tried. With `--rootfs`, `/etc/hosts` and
`/etc/resolv.conf` are written into that directory, since it is the
container's filesystem (with an image they go into the container's own
layer). Nothing about networking was measured: no throughput or latency
figures exist.

## Security

Before the command is exec'd, Init hardens it. Everything privileged (the
mounts, the hostname, the network) is done first, with full capabilities;
then, on a locked OS thread so the fork for the command inherits exactly
this state, Init cuts capabilities, sets `no_new_privs`, and installs a
seccomp filter.

```bash
minibox run alpine:3.22 sh         # a small capability set, seccomp, masked /proc
minibox run --userns alpine:3.22 sh # and its own user namespace: root here is nobody on the host
```

- **Capabilities: Docker's default set, minus two.** The command keeps the
  twelve capabilities a normal program expects (file permissions,
  setuid/setgid to drop to a user, chroot, kill, binding low ports,
  audit). MiniBox drops two that Docker keeps:
  - **`CAP_NET_RAW`** — every bridged container shares one bridge with no
    per-container packet filtering, so a raw or packet socket could sniff
    or spoof its neighbours. The cost: `ping` does not work.
  - **`CAP_MKNOD`** — MiniBox has no devices cgroup, so a device node the
    container made (`mknod /dev/sda b 8 0`) would read the host's disks.
    Docker keeps `CAP_MKNOD` only because it pairs it with a devices
    cgroup that denies access to anything so created.

  The whole set is dropped from the **bounding set** too, so no capability
  can be regained by exec'ing a set-user-id or file-capability binary. A
  command that runs as a non-root user (`-u`) keeps **no** capabilities at
  all — the point of running as a user, and what Docker does.
- **`no_new_privs`** stops a set-user-id or file-capability binary the
  command exec's from gaining anything beyond that set, and lets the
  seccomp filter load without `CAP_SYS_ADMIN`.
- **A seccomp filter written by hand** (`internal/container/security_linux.go`):
  a classic-BPF program, not libseccomp. It reads the architecture from
  `seccomp_data` and kills the process if it is not this build's, so a
  second ABI cannot reach the kernel with system-call numbers the filter
  never checked; then it returns `EPERM` for a hand-picked subset of
  Docker's denied calls — loading or unloading kernel modules, `kexec`,
  `open_by_handle_at`, `perf_event_open`, `reboot`, swap, and the kernel
  keyring (`add_key`, `keyctl`, `request_key`). Most are also blocked by a
  missing capability; the filter is a second line. The keyring calls need
  **no** capability, so there the filter is the only thing stopping them —
  the test proves it.
- **Masked and read-only paths,** as Docker does: `/proc/kcore`,
  `/proc/keys`, the timer and scheduler files, `/sys/firmware` and the
  rest are covered (a file with `/dev/null`, a directory with an empty
  read-only tmpfs); `/proc/bus`, `/proc/fs`, `/proc/irq`, `/proc/sys` and
  `/proc/sysrq-trigger` are made read-only, so a process cannot retune or
  crash the host kernel. These are set up inside the root filesystem
  before `pivot_root`.
- **`--userns`: an optional user namespace.** The container's ids 0-65535
  map to host ids 100000-165535, so the container's root is an
  unprivileged user on the host — if it escaped the other controls it
  would still own nothing outside the namespace. `minibox run` is real
  root, so it writes the mapping directly (no `newuidmap`, no
  `/etc/subuid`). The device nodes are bind-mounted from the host's `/dev`
  rather than created with `mknod`, which a user namespace forbids. The
  capability, `no_new_privs` and seccomp hardening still applies, so even
  the namespace's root cannot mount or load a module.

**Known gaps — this is a learning runtime, not a sandbox for hostile
code:**

- **No devices cgroup** (so `CAP_MKNOD` is dropped rather than contained)
  and **no AppArmor or SELinux profile.**
- **The user namespace does not id-map the root filesystem.** The
  rootfs's files stay owned by the host's root, which is unmapped inside,
  so the container's root sees them as `nobody` and cannot modify a
  root-owned file; `/etc/hosts` and `/etc/resolv.conf` are left as the
  image shipped them (the network is still set up). The fix is an
  id-mapped mount (`mount_setattr` with `MOUNT_ATTR_IDMAP`), which needs a
  helper user namespace and a mount moved across the clone boundary — not
  built. Without `--userns` this does not arise (Init is real root).
- The seccomp denial list is a subset of Docker's, chosen for the calls
  with a clear cost and no use in an ordinary program; it is not the whole
  profile, and it has no per-argument rules.

## The OCI runtime lifecycle

`minibox run` is MiniBox's own one-shot command. MiniBox also speaks a
subset of the **OCI runtime lifecycle**, the split create/start model
that `runc` and every container engine use, on a **bundle** — a directory
with a `config.json` (the OCI runtime spec) and a root filesystem:

```bash
cd mybundle                    # a config.json and a rootfs/, e.g. from `runc spec`
minibox create --bundle . c1   # set it up; its process is not running yet
minibox state c1               # -> {"status":"created", "pid":..., ...}
minibox start c1               # run the process
minibox state c1               # -> running, then stopped when it exits
minibox kill c1 TERM           # signal it
minibox delete c1              # remove a stopped container
```

- **Create sets up, start runs.** `create` starts the container's init in
  all its namespaces and its cgroup, sets up the root filesystem and
  network, and then **blocks**, so the container is fully "created" but
  its command has not run. `start` releases it. The gate is an **exec
  fifo** (as runc uses): init opens it before `pivot_root` detaches the
  host's filesystem and blocks reading it; `start` writes one byte. Init
  then **execs the command in its own place**, so the command is PID 1 —
  the runc model, not `run`'s init-stays-PID-1-and-supervises model.
- **Detached.** `minibox create` returns once setup has succeeded; the
  container is reparented to the host's init and lives on its own, with
  its state in `/run/minibox/oci/<id>/` (a `state.json`, the fifo, and the
  process's `output.log`). `state` recomputes "stopped" from whether the
  process is still alive.
- **What it reads from config.json:** the process (args, env, cwd, user,
  no terminal), the root path, the hostname, which namespaces to make
  (uts/pid/mount/ipc/cgroup are always isolated; a network namespace is
  made or the host's is shared per the config; a user namespace is
  honored; joining an existing namespace by path is refused), and the
  cgroup resources (memory, cpu, pids).
- **What it does not read, on purpose** (so this is a lifecycle subset,
  not a full OCI runtime): the config's mounts, masked/readonly paths,
  capabilities, seccomp, rlimits, hooks and annotations. MiniBox applies
  its own standard mounts and its own Milestone-5 hardening regardless.
  Running an **image** under the OCI lifecycle, or `--userns` with an
  image, is refused — the image store is the host root's, which a mapped
  container root cannot read; use a bundle directory.

### Checked against runc

`scripts/oci_compare.sh` (`make oci-compare`) runs the **same bundle**
under MiniBox and under runc and compares what the process produced and
how the container moved through its states. runc is a reference only,
never the engine. On 2026-10-13, Ubuntu 24.04 in the VM, runc 1.3.4: **8
of 8 bundles matched** — `echo`, `id -u`, `hostname`, the working
directory, `$PATH`, a written file read back, a clean exit, and stdout
with stderr all produced identical output and the same create → running →
stopped lifecycle. Because MiniBox ignores the config's capabilities,
mounts and seccomp, the agreement is on lifecycle and process behaviour,
not on those.

### Start-up time

`bench/oci_startup.sh` (`make oci-bench`) times `create` then `start`
against runc on the same bundle. Quotable only with the caveat **one
2 GiB Lima VM on one 8 GB M2 laptop; the command is `true`, so this times
the runtime's own work, not a workload; runc applies the whole config
(all mounts, capabilities, a libseccomp filter) while MiniBox applies its
subset, so it is an order-of-magnitude comparison, not a controlled one.**
Median of 30, three passes, 0 swap-ins (a clean run):

| | create | start | total |
|---|--------|-------|-------|
| MiniBox | 9-10 ms | 5 ms | **14-15 ms** |
| runc | 25-26 ms | 4 ms | **29-30 ms** |

MiniBox's `create` is the faster of the two, which fits — it does less
than runc (no capability or seccomp parsing from the config, fewer
mounts). `start` is a fifo write and an `execve` in both, and is about the
same.

## How a container starts

```
minibox run ──► Run: checks the config, makes the cgroup, locks its OS thread, starts
   /proc/self/exe init with CLONE_NEWUTS|NEWPID|NEWNS|NEWIPC|NEWCGROUP (and NEWNET unless
   --network host, NEWUSER if --userns), inside the cgroup, a session of its own, Pdeathsig=SIGKILL
   ──► attaches Init's network namespace to the bridge (a lease, the veth pair, ports)
   ──► sends the config as JSON on fd 3 ──► waits for one status message on fd 4
minibox init (PID 1 in the new namespaces) ──► / made a private mount, so nothing travels back
   ──► the root filesystem bind-mounted on itself; /proc, a /dev of its own, /sys read-only and
       the masks mounted inside it; then pivot_root(".", ".") and the old root detached
   ──► the hostname ──► loopback, eth0, the route, /etc/hosts and /etc/resolv.conf
   ──► on a locked thread: capabilities cut, no_new_privs, the seccomp filter ──► the command
       started (found in the container's PATH), inheriting that ──► "started" on fd 4
   ──► until the command exits: every exit in the container reaped, forwarded signals passed on
   ──► exits with the command's status; the kernel kills whatever is left in the container
```

- **Why a second process ("re-exec"):** namespaces are entered by
  `clone`, for a new process. The Go runtime is already several threads
  by the time `main` runs, so it cannot `unshare` itself into a new
  mount or PID namespace; starting a fresh copy of the binary with clone
  flags is how Go programs do it.
- **PID 1 is special:** the kernel gives it every orphaned process to
  reap, and does not apply default signal actions to it. So Init is not
  the command: it waits with `wait4(-1)` for everything, in one place (a
  second waiter could take the command's status), and passes `SIGHUP`,
  `SIGINT`, `SIGQUIT`, `SIGTERM`, `SIGUSR1`, `SIGUSR2`, `SIGWINCH` and
  `SIGALRM` on to the command. Not every signal: the Go runtime sends
  itself `SIGURG` to preempt goroutines, and `SIGCHLD` is how Init hears
  of exits.
- **The container dies with `minibox run`:** `Pdeathsig` kills Init if
  `minibox run` dies, and with PID 1 gone the kernel kills the rest. In Go
  this needs the starting OS thread locked for the container's lifetime:
  `Pdeathsig` fires when that *thread* exits, not the process. (strace
  shows a side effect: Go's own check for a parent that died already
  compares `getppid()` with the parent's pid, and in a new PID namespace
  `getppid()` is 0, so the new Init is sent `SIGKILL` by itself. The kernel
  ignores it: a namespace's PID 1 does not take `SIGKILL` from inside.)
- **A session of its own:** a terminal's Ctrl-C goes to `minibox run`,
  which passes it on once, rather than to every process at the same time.
- **The host's environment never reaches the container:** Init starts
  with an empty environment, and the command gets `PATH`, `HOME`,
  `HOSTNAME`, `TERM` (if set) and the `-e` variables.

### What the container gets

| | |
|---|---|
| Namespaces | its own UTS (hostname), PID, mount, IPC, cgroup and network (unless `--network host`), and its own user namespace with `--userns`; otherwise the host's users |
| `/` | the root filesystem directory, through `pivot_root`, with the old root detached |
| `/proc` | its own (only its processes), `nosuid,nodev,noexec`; the host-revealing paths masked, the kernel-tuning paths read-only |
| `/dev` | a tmpfs with `null`, `zero`, `full`, `random`, `urandom`, `tty`, `fd`, `stdin`/`stdout`/`stderr`, `pts` (a new devpts instance), `ptmx`, `shm`, `mqueue`; none of the host's disks |
| `/sys` | read-only |
| Network | `lo` and `eth0` with its own address on the bridge, or only `lo` (`--network none`) |
| Capabilities | a small set (see Security), `no_new_privs`, a seccomp filter; none at all when it runs as a non-root user |
| Files open | 0, 1 and 2 only: Init's pipes to `minibox run` are closed on exec |

A top-level `/proc`, `/dev` or `/sys` that is a symbolic link or a file is
refused; the special filesystems are mounted into the root filesystem
before `pivot_root` (a fresh procfs or sysfs can be mounted in a user
namespace only while a fully visible one is still in the tree), and every
other path then sits inside one of those new mounts, so a link in the
image cannot redirect a mount out of the root filesystem.

**Not yet:** the command has no terminal of its own (`-t`): an interactive
shell works, without job control. And see Security for what the hardening
does not cover (no devices cgroup, no LSM profile, no id-mapped rootfs).

## How it is developed

- **Built on the Mac, tested in the VM.** `make vm-test` copies the source
  into `~/.cache/minibox-dev`, the only folder the VM shares with the Mac,
  and runs the checks there as root. The project folder itself is not
  shared: it is synced by iCloud, which moves files off the disk and
  stalls whoever reads them.
- **The VM is pinned:** Ubuntu's image by SHA-256 (Lima's template), Go
  1.27.1 by go.dev's SHA-256, installed by the VM's provisioning script
  with gcc (for `go test -race`) and nftables. No containerd.
- **The test root filesystem** is Alpine's minirootfs 3.22.6 (4 MB),
  downloaded by `scripts/rootfs.sh` into the same cache and checked against
  its pinned SHA-256; tests find it through `MINIBOX_TEST_ROOTFS`.

## How it is tested

- **In the VM and in CI the tests run as root with
  `MINIBOX_TEST_REQUIRE=1`,** which turns a test that would skip for lack
  of Linux or root into a failure: a suite that skips everything must not
  pass.
- **Isolation, from inside real containers** (`internal/container`, on
  Alpine's minirootfs): the four namespaces differ from the host's and the
  other three do not; the hostname is the container's and the host's is
  untouched; only the container's processes are visible, with Init as PID
  1 leading its own session; `/` is the root filesystem and `cd /..` stays
  there; exactly the seven expected mounts; `/dev` holds exactly the
  expected entries and they work as devices; `/sys` refuses writes; exit
  codes (0, 3, 137, 143, 127 three ways, 126 two ways); an orphan that
  exits is reaped (no zombie); trapped and untrapped signals reach the
  command; a mount made inside never shows on the host, checked while the
  container runs (where `/` is a shared mount, which the test checks
  first); exactly the `-e` variables and none of the host's; only fds 0, 1
  and 2 open; a `/proc` linked to the host's `/etc`, a file `/sys`, a
  linked or missing root filesystem all refused.
- **An escape attempt:** a small static program (`internal/testutil/probe`)
  tries the classic chroot escape from inside, and must stay in the
  container. With `chroot` in place of `pivot_root`, or the old root left
  attached, it gets out, and the test fails.
- **Images, against a fake registry** (`internal/testutil/registry.go`: an
  HTTP server that answers like Docker Hub, with a bearer challenge, an
  index listing this machine's manifest and an s390x one, and blobs by
  digest): one token for many requests; this platform chosen; a manifest,
  a config or a layer whose bytes do not match its digest, a layer cut
  short, a config whose diff_id is not the layer's, a manifest that
  misstates a size, an image for another architecture, and a layer that
  escapes are each refused, and leave no layer, no temporary folder and no
  image behind; layers shared between images are fetched once and kept
  when one image is removed.
- **Unpacking, against hostile layers:** `..` names, absolute names, files
  written through absolute, relative and chained links, a file written
  over a link, hard links out of the layer, through a link, and to a link
  that points out: the host's file is never changed, nothing is added next
  to it, and no file in the layer is the host's (same inode). Whiteouts,
  opaque directories, a forged `trusted.overlay.opaque` (not applied),
  owners, set-user-id bits, times, devices and FIFOs, entries replacing
  each other, the size limit. And 300 random layers made of awkward names,
  links pointing anywhere (in loops too) and hard links: whatever is
  accepted or refused, nothing outside the layer changes.
- **Against GNU tar on real images** (`scripts/check_extraction`, a
  development check that pulls from Docker Hub): each layer unpacked by
  MiniBox and by `tar --xattrs`, and the trees compared entry by entry
  (type, mode, owner, size, content, hard-link count, link target, device
  numbers, extended attributes). alpine:3.22, busybox:1.37,
  debian:trixie-slim, python:3.14-slim and nginx:1.29-alpine on
  2026-10-10: 18 layers, 12,252 entries, 0 differences. None of those
  layers held a whiteout, so whiteouts are checked only by the tests
  above.
- **Images end to end** (`cmd/minibox`): an image made of Alpine's
  minirootfs, a layer with a script and a whiteout for `/etc/motd`, and a
  layer making `/opt` opaque, pulled with `minibox pull` from the fake
  registry and run: entrypoint and command (and `--entrypoint`), the
  image's user, groups, working directory (made) and environment, `-u`,
  `-w`, `-e`; the whiteout and the opaque directory applied; a write and a
  deletion landing in the container's own layer (`--keep`), the image's
  layers untouched and a second container seeing the image as it was; the
  container's folder removed without `--keep`; no overlay mounted on the
  host while the container runs or after; `images`, and `rmi` removing the
  layers.
- **Limits, in real containers:** the container reads its own limits at
  `/sys/fs/cgroup` (and cannot write them) and sees its group as `/`; 200
  MiB allocated under a 64 MiB limit is killed (137, one OOM kill counted)
  while 24 MiB runs and its peak is read back; 100 processes asked for
  under a limit of 32 start fewer than 32 and the refusals are counted; a
  real fork bomb (`f() { f | f & }; f`) under a limit of 64 is contained,
  the shell that started it carries on, and nothing of it is left; half a
  CPU for 3 s of spinning uses about 1.5 s; limits too small are refused;
  every container's group is gone afterwards; a group left empty by a
  killed `minibox run` is swept once it is a minute old, a fresh one is
  not; and in a parent group made fresh, MiniBox enables the controllers
  itself. Through the CLI: the OOM message, `--stats`, and bad flags (125).
- **Networking, in real containers:** eth0 has an address in
  10.99.0.0/24 other than the bridge's and is up, the default route goes
  through the bridge, `/etc/hosts` names the container and
  `/etc/resolv.conf` has no loopback nameserver; a container serving HTTP
  is reached by another through its address; a published port answers on
  the host's `127.0.0.1` and on its own address, from **another machine**
  (a network namespace on a veth to the host, standing for one: it must go
  through the host's prerouting chain), and from **another container**
  through the host's address on the bridge; the same port cannot be
  published twice, and it stops answering, its rule gone, when the
  container ends; the same other machine is reached from a container only
  through masquerading (it has no route back to the bridge); eight
  containers started at once get eight different addresses and give them
  back with their veths; `--network none` has only `lo`, and it works;
  `--network host` shares the host's namespace and refuses `-p`; a lease
  whose holder died, or whose holder's pid now belongs to another process,
  is taken back by the next container with its port; and when `minibox
  run` is killed outright, its veth disappears with the container and its
  lease is taken back by the next run. Without containers, against
  processes in network namespaces of their own: four attaches held between
  reading and writing the leases still get four addresses (the lock is all
  that keeps them apart); detaching gives back the lease, the veth and the
  port; a port held by a TCP listener or a bound UDP socket on the host is
  refused, and published once it is let go. Netlink messages, attributes
  and their padding and nesting, socket tables, and `-p` and `--network`
  parsing have unit tests of their own.
- **Security, in real containers:** the command's effective capabilities
  and its bounding set are exactly the expected mask, `no_new_privs` is
  set and seccomp is in filter mode; a non-root command (`-u`) has no
  capabilities; mounting, `open_by_handle_at`, loading a kernel module,
  `mknod` of a block device, a raw or packet socket, and a write to
  `/proc/sysrq-trigger` are each refused, and so is `keyctl` (which needs
  no capability, so only the filter stops it — it shows the filter does
  something the capability drop does not); `/proc/kcore` reads empty while
  `/proc/sys` still reads. With `--userns`: the container's root is uid 0
  inside, the uid map is 0 → the host's unprivileged range, a bind-mounted
  `/dev/null` works, and mounting is still refused (the hardening does not
  depend on the user namespace); and running an image under `--userns`
  is refused (the image store is the host root's).
- **The command end to end** (`cmd/minibox`), with the test binary playing
  `minibox`: the default hostname is the id, usage errors are 125, Ctrl-C
  sent to `minibox run` ends the container with 130, and killing `minibox
  run` with `SIGKILL` takes the container with it.
- **The OCI lifecycle** (`internal/oci`): a bundle is created (state
  "created", a pid), started, runs its command, reaches "stopped", and is
  deleted; a created container is deleted before it is ever started;
  starting a running container, killing a stopped one, and deleting a
  running one without `--force` are each refused; `kill` ends a running
  container; the config's working directory and cgroup memory limit are
  applied; a command that does not exist fails at create. The spec reader
  is unit-tested apart from the VM: the root path, hostname default, user,
  env, network-namespace-to-mode and resource mapping, and the refusals
  (a terminal, no args, no root, a namespace path, a too-small memory
  limit).
- **118 planted bugs** each fail the tests: 5 in Milestone 0 (failures not
  counted, filesystem names read from the wrong field, missing items
  inverted, cgroup v1 accepted, `check` exiting 0 on a failure), 16 in
  Milestone 1 (mounts not private, `chroot` for `pivot_root`, the old root
  not detached, only the command reaped, signals not forwarded by Init or
  by Run, signal deaths not 128+n, Init's pipes inherited, no IPC
  namespace, no `Pdeathsig`, `/sys` writable, the host's environment
  passed in, "not found" as 125, a linked mount point followed, no
  session of its own, no hostname) and 23 in Milestone 2 (`..` allowed,
  links followed out of the layer, a file written through a link in its
  place, hard links following a link, a layer's `trusted.*` attributes
  applied, whiteouts written as files, opaque directories not marked, blob
  digests, blob sizes, manifest digests or diff_ids not checked, a layer
  renamed into place before it is checked, layers in the wrong order, the
  platform's architecture ignored, the token not kept, the image's user or
  entrypoint ignored, the size limit not enforced, shared layers removed,
  `HOME` not from passwd, mode set before owner, directory times never set,
  the working directory not made) and 15 in Milestone 3 (memory or pids
  limit not written, swap allowed by default, the CPU quota off by ten,
  Init not born in its group, no cgroup namespace, the container's cgroup
  writable, the group not removed, OOM kills read from the wrong field,
  fresh groups swept, tiny memory limits allowed, controllers not enabled
  in the parent, megabytes as 1,000,000, the OOM not reported, usage read
  after the group was removed) and 35 in Milestone 4 (attributes not
  padded or not marked nested, message lengths unchecked, the kernel's
  errors ignored, the veth's peer left on the host, the host's end not on
  the bridge or not up, loopback or eth0 not up inside, no default route,
  each of the three masquerade rules and both DNAT rules removed,
  `route_localnet` or forwarding not turned on, the lease file not locked,
  every container given the first address, ports or leases kept after the
  container ends, dead holders' leases kept, a pid alone deciding a holder
  is alive, a port published twice, loopback nameservers kept, `/etc/hosts`
  or `/etc/resolv.conf` not written, no network namespace, one for
  `--network host`, ports allowed off the bridge, port 0 accepted, Run
  never detaching, a port in use on the host not checked, bound UDP
  sockets not seen, socket table ports read as decimal) and 11 in
  Milestone 5 (the bounding set not dropped, `no_new_privs` not set, the
  seccomp filter not loaded, `keyctl` left on the allowed list, each of
  `CAP_SYS_ADMIN`, `CAP_NET_RAW` and `CAP_MKNOD` kept, the masked paths
  not masked, the read-only paths not sealed, the user namespace not
  entered as root, its devices not bind-mounted) and 13 in Milestone 6
  (the exec fifo opened read-only, the working directory not set before
  the exec, `start` writing nothing to the fifo, `start` allowed when not
  created, `state` never recomputing stopped, `delete` not requiring
  stopped, a zombie treated as alive, and in the spec reader: a terminal,
  a namespace path or a too-small memory limit not refused, a network
  namespace giving host networking, the hostname not defaulting to the id,
  a non-root user ignored, the memory limit not mapped). Eight were caught
  only after a fix (failures 1-3, 7 and 14); two of Milestone 6's were
  caught only by the suite's five-minute timeout (a blocking fifo open and
  a blocking fifo open-for-write), the rest by a specific test.
- **The smoke test** (`make smoke`, 51 checks): a static Linux binary,
  `check` passing as root in the VM with no warnings, failing as an
  ordinary user and saying why, failing on the Mac and saying Linux is
  needed, usage errors exiting 2; real containers on a folder: their own
  hostname, Init as PID 1 with no host process visible, exit codes 7 and
  127 passed back, 125 without root, nothing left running or mounted; and
  a real image: `minibox pull alpine:3.22` from Docker Hub, `images`,
  Alpine 3.22 inside, a write that stays in its container, no container
  folder or overlay left, `rmi` removing the layers, 125 for an image that
  is not there; and limits on that image: a 64 MiB buffer stopped by
  `--memory 32m` with the message, a fork bomb contained by `--pids 32`,
  the container reading its own `memory.max`, no cgroup left; and
  networking: an address on the bridge, the internet reached by name from
  a container (DNS and NAT), only loopback with `--network none`, and a
  real `nginx:1.29-alpine` answering on a published port, stopped by
  SIGINT, after which the port is closed, its rule gone, and no veth or
  lease is left; and security: the container cannot mount or ping (caps
  dropped), `no_new_privs` and seccomp are on, `/proc/kcore` is empty and
  `/proc/sysrq-trigger` is read-only, and a `--userns` container is uid 0
  inside, maps to the host's unprivileged range, and the host sees its
  process running as that high uid; and the OCI lifecycle on a bundle:
  `create` leaves it "created", `start` runs it, it reaches "stopped" with
  the right output, `delete` removes it, and a bad bundle fails.
- Each container in the tests takes about a second: the race detector
  sleeps 1 s when a process exits with status 0, and every Init is a copy
  of a race-enabled test binary.

## Failures found while building it

1. **A test that could not fail.** In the VM every host check passes, so
   the test of `check`'s exit code only ever saw success, and a planted
   "exit 0 on failure" passed it (the smoke test caught it). The test now
   runs `check` against reports that fail and that only warn.
2. **A planted bug looked caught for the wrong reason.** Removing the
   separate session for the container failed the tests, but not because
   of the session: an earlier planted bug had stopped signals reaching a
   container, the test timed out, and that test's `minibox run` (a process
   of its own) outlived the test binary with its container still running
   `sleep 3021`. The next run's test found that old sleep, signalled its
   own `minibox run` before it was ready, and failed. Fixed in the tests: a
   `minibox run` started by a test now dies with the test binary
   (`Pdeathsig`), each run uses a random sleep, every signal test ends by
   itself within about 10 s, the suite has a 5-minute timeout, and a new
   test checks the session directly (Init's session is 1 inside; 0 without
   one of its own). Re-run: all four signal and session plants caught,
   nothing left running.

3. **Three planted bugs in the image code passed at first.** Two were not
   bugs: unpacking removes whatever is at a name before writing it, so
   writing a file without `O_NOFOLLOW` still could not go through a link
   in its place; and directory times are set again at the end, so setting
   them early as well changed nothing. Each was replaced by a plant that
   removes both guards, and both are caught. The third was a gap: no test
   had a manifest that misstates a blob's size (the digest alone rejects a
   blob cut short or padded), so the size check was never exercised; a
   test now serves one.
4. **A test hung for five minutes, on a promise overlayfs does not make.**
   To release a waiting container, a test wrote a file into the
   container's upper folder from the host; the container never saw it.
   Changing an overlay's layers while it is mounted is undefined in the
   kernel's documentation, and here the new file did not show. The test
   now releases the container through its stdin.
5. **An old test became wrong once packages took longer.** "No container
   left running" looked for any Init in the VM, but Go runs packages'
   tests at the same time, and the image tests' containers were still
   running. It now counts only Inits whose parent is its own test process.
6. **Every test container had the same id.** The container tests named
   every container "test", which was harmless until a container's cgroup
   was named after its id: two containers at once (eight in a later test,
   or a second test run in the VM) then collided on
   `/sys/fs/cgroup/minibox/test`. Ids must be unique, and the tests now
   draw a new one each time. The first planted-bug run of the limits
   overlapped a run that collided like this, so its results were thrown
   away and all 15 run again.
7. **A planted bug hidden by what an earlier run left behind.** Not
   enabling the controllers in MiniBox's parent group passed every test:
   the VM still had that parent from earlier runs, with the controllers
   on. A test now makes a parent of its own, so the step is needed.
8. **The smoke test found a group left behind, but only sometimes.** "No
   container cgroup is left" failed in 3 smoke runs and passed in others,
   and no replay of its own steps left a group. The group came from the
   test suite run just before: the test that kills `minibox run` outright
   leaves that run's group, empty (a killed process cannot clean up), for
   the next run to sweep a minute later, and the smoke test started within
   that minute. The test now checks that the killed container's group is
   empty and removes it. Two changes made on the way stay: removing a
   group is retried while the kernel says it is busy, and a removal that
   fails is reported rather than ignored.
9. **A merge that would have undone fixes.** The networking was first
   written in a scratch copy of the project, made before Milestone 3's
   last fixes. Copying it over would have quietly removed them (the busy
   retry and error report on group removal, the kill test's cleanup, and a
   Milestone 2 test), so it was merged change by change.
10. **Containers could not reach each other's published ports through the
    host's address.** Seen while reviewing the rules, then shown by a test
    written for it, which timed out: the request is sent back into the
    bridge it came from (DNAT), and the answer goes straight back across
    the bridge, from the serving container's address, to a container that
    had written to the host's. It is dropped. Docker calls the fix hairpin
    NAT; here it is masquerading connections that were DNAT'd and leave
    through the bridge, so the answer comes back through the host.
11. **Three faults found by reading, not by a failing test.** A lease's
    holder was recognised by its pid alone, and pids are reused, so a new
    process could hold a dead container's address forever (now the start
    time too, tested with a lease whose pid is alive but started at another
    time). The function that gives back a container's network would have
    reported a failure into a variable of its own (`att, err :=` inside a
    `switch` makes a new `err`), so it would never have been seen (no test
    makes that function fail; a comment guards it). And a test rewrote the
    lease file without its lock while other packages' containers were
    starting (it takes the lock now).
12. **A smoke check that could not fail.** "The port is closed again"
    passed even if the port's rule had stayed behind, since a connection
    to an address that no longer exists fails too. It now also checks that
    the rule is gone.
13. **The planted-bug runs killed their own reset.** Each run first resets
    the VM's network state, because the rules are written only when the
    table is missing (otherwise a planted rule would be tested against the
    old ones). The reset looked for leftover containers with `pgrep -f
    minibox-init` and killed them, which matched the reset's own shell and
    killed it before it deleted anything; the fix (`[m]inibox-init`) still
    matched, through the shell's own `echo "... minibox-init ..."`. Both
    runs were thrown away. Stopping the first one also left a planted bug in
    the source, found by checking every plant's text afterwards; the runner
    now puts the file back when it is stopped.
14. **Two planted bugs passed the networking tests at first,** both gaps
    in the tests. Not giving a lease back passed because the test compared
    addresses as `ip` prints them, `10.99.0.2/24`, with the lease file's
    `10.99.0.2`: "still leased" could never be true. Not locking the lease
    file passed because eight containers starting at once never happened
    to read and write the file at the same moment. The first test now
    compares bare addresses and checks they are addresses; a new test
    holds four attaches open between reading and writing the leases (a
    hook only tests set), so without the lock they collide every time.
    Both plants are caught on a re-run, with three new ones for the
    port-in-use check. (The first plant of the full run also met two
    containers left by the stopped run's tests, still finishing inside the
    VM; the reset cleared them, and that plant is caught by a unit test
    that does not depend on the VM.)
15. **The seccomp filter was rejected with EINVAL.** Each compare in the
    hand-built BPF program jumps forward to the "deny" return, and the
    jump distance was computed as `errnoIndex - (len(filter) + i) - 1` —
    but `len(filter)` already grows as instructions are appended, so the
    index was counted twice and the jumps pointed past the end of the
    program, which the kernel refuses. The landing index of a new
    instruction is just `len(filter)`; with that the offsets are right.
    Guards: the capabilities test checks the filter is installed (seccomp
    mode 2), and the `keyctl` escape test checks a denial actually fires
    (keyctl needs no capability, so only the filter can stop it).
16. **User namespaces took three goes, and one limit remains.** (a) The
    container's root showed as `nobody` and could not mount or set up its
    network: its host uid was outside the mapped range, so it was
    unmapped, and the capabilities it held as the namespace's creator were
    lost at the exec into `minibox-init` (that exec ran as the unmapped,
    non-root uid). Fixed by having Go change to uid 0 — via a `Credential`
    on the clone — after it writes the id maps and before that exec, so
    Init runs as the namespace's root with its capabilities. (b) Then
    mounting `/proc` failed: a fresh procfs may be mounted in a user
    namespace only while a fully visible one is still in the mount tree,
    and MiniBox mounted it after `pivot_root` had detached the host's.
    Fixed by mounting `/proc`, `/dev` and `/sys` into the root filesystem
    *before* pivoting (which also let the device nodes be bind-mounted
    straight from the host's `/dev`). (c) The remaining limit — the root
    filesystem is not id-mapped, so its root-owned files are `nobody`
    inside and the container's root cannot rewrite `/etc/hosts` — is
    documented in Security, not fixed; the file writes are best-effort in
    a user namespace.
17. **The hardening broke tests that assumed an all-powerful container.**
    Dropping `CAP_NET_RAW` stopped `ping` (used by two network tests to
    check reachability); dropping `CAP_SYS_ADMIN` stopped a command
    changing the hostname or mounting; and the new masks added mounts a
    test counted exactly. These were the hardening working: the tests were
    updated to the new behaviour (reachability is checked over TCP by the
    container-to-container and NAT tests; the interface is checked for its
    up state; a command's mount and hostname change are asserted to be
    refused; the extra mounts are checked to be only `/proc` and `/sys`
    masks). The behaviour matches Docker, whose default also drops
    `CAP_SYS_ADMIN` and whose `--user` default adds no capabilities.
18. **The OCI container never started: the fifo read saw EOF.** The exec
    fifo opened read-only returns end-of-file from a read as soon as there
    is no writer — and at `create` time there is none, so the init woke
    immediately and exited "deleted before it started". A fifo opened
    `O_RDWR` holds a write end too, so the read blocks for data instead;
    `start`'s byte then wakes it. (The open does not block on Linux with
    `O_RDWR`.)
19. **The OCI command ignored its working directory.** The comparison with
    runc showed `pwd` returning `/` where runc gave `/tmp`: the OCI path
    `execve`s the command, and `execve` keeps the current directory,
    unlike the `run` path's fork which sets it through `ProcAttr.Dir`. Init
    now `chdir`s to the config's `cwd` before the exec. A test runs `pwd`
    with a non-root `cwd` to guard it.
20. **In the tests the OCI container never reached "stopped".** The unit
    tests call the create logic in-process, so the container is the test
    process's child; when it exits the test does not reap it, so it lingers
    as a zombie, and a zombie still answers "signal 0" — the liveness
    check read it as running forever. In production `minibox create` is
    its own short-lived process, so the container is the host init's child
    and is reaped. The fix makes both honest: liveness reads `/proc/<pid>/
    stat` and treats a zombie as stopped, and `delete` reaps the process
    if it is its own child (a harmless no-op for the real command).

## Layout

| Path | What |
|------|------|
| `cmd/minibox/` | The command line and its exit codes; `oci.go` is the create/start/state/kill/delete commands; end-to-end tests of `run` |
| `internal/oci/` | The OCI lifecycle: `spec.go` reads the config.json subset into a container.Config; `state.go` the on-disk state store; `runtime_linux.go` create/start/state/kill/delete; `container.CreateDetached` and the exec fifo are its engine |
| `internal/container/config.go`, `lookpath.go` | A container's configuration and environment; finding the command in the container's PATH |
| `internal/container/run_linux.go` | `minibox run`'s side: starting Init in new namespaces, the config and status pipes, passing signals on, exit codes |
| `internal/container/init_linux.go` | Init, PID 1: setting up, starting the command, reaping, forwarding signals |
| `internal/container/rootfs_linux.go` | Private mounts, `/proc`, `/dev`, `/sys` and the masks, `pivot_root`, user-namespace device bind-mounts |
| `internal/container/security_linux.go`, `seccomp_{arm64,amd64}.go` | Capabilities, `no_new_privs`, the hand-written seccomp filter, the per-architecture audit value |
| `internal/image/reference.go`, `oci.go` | Image names as Docker reads them; OCI and Docker manifest types; choosing this machine's platform |
| `internal/image/registry.go` | The registry client: bearer tokens, manifests and blobs, every digest checked |
| `internal/image/extract_linux.go` | Unpacking a layer safely, with whiteouts as overlayfs reads them |
| `internal/image/store.go` | The store: pull, list, remove, a container's overlay folders |
| `internal/cgroup/` | A container's cgroup v2 group: limits, usage read back, removal, the sweep of stale groups |
| `internal/netlink/` | Route netlink by hand: messages and attributes, links (bridge, veth), addresses, routes |
| `internal/network/` | The bridge, leases, veth pairs, nftables NAT and published ports (host side); loopback, eth0, route, `/etc/hosts` and `/etc/resolv.conf` (inside) |
| `bench/limits.sh`, `bench/results/` | The limits measured in the VM |
| `internal/container/user.go` | Users and groups from the container's own `/etc/passwd` and `/etc/group` |
| `internal/testutil/` | Root required (or a skip), the test root filesystem, the probe (escape, memory, CPU, processes, HTTP server and client), a fake registry and layer builders |
| `scripts/check_extraction/` | Unpacking real images by MiniBox and by GNU tar, compared |
| `scripts/oci_compare.sh`, `bench/oci_startup.sh`, `scripts/vm-oci.sh` | The OCI lifecycle checked against runc on the same bundles, and start-up timed against it (dev-only) |
| `internal/host/` | `minibox check`: what the machine has, checked one item at a time |
| `lima/minibox.yaml` | The development VM |
| `scripts/vm-test.sh` | The checks, in the VM, as root |
| `scripts/rootfs.sh` | The pinned test root filesystem |
| `scripts/smoke.sh` | The binary, checked from outside |
| `.github/workflows/ci.yml` | CI on Ubuntu 24.04 (written; it has not run) |
