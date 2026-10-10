# MiniBox

A container runtime built from the Linux kernel's own pieces: namespaces,
`pivot_root`, cgroup v2, overlayfs, virtual network devices, capabilities
and seccomp. The point is to know what Docker and runc do underneath, so
none of runc, containerd, libcontainer, go-containerregistry, CNI plugins,
netlink libraries or libseccomp is used; `golang.org/x/sys/unix` (system
call wrappers) is allowed, and runc may appear in tests as a reference.

> **Status:** Milestones 0-2 of 7 built, on my machine (no GitHub
> repository yet, so CI has not run). `minibox pull alpine:3.22` fetches an
> image from its registry, checking every digest, and unpacks its layers
> safely; `minibox run alpine:3.22 sh` runs it on an overlay of those
> layers, with its own hostname, process ids, mounts and IPC, under an
> init that reaps and passes signals on. **It is not a security boundary
> yet:** the command is root with every capability (unless the image or
> `-u` names a user), and shares the host's network and users, until
> Milestones 4 and 5.

> **Where it runs:** Linux only, as root. On my Mac that is a Lima VM
> (`lima/minibox.yaml`: Ubuntu 24.04, 2 CPUs, 2 GiB, 10 GiB disk); in CI,
> GitHub's Ubuntu runners. From Milestone 4 MiniBox changes the network
> setup of the machine it runs on (a bridge, nftables rules), so it belongs
> in a disposable VM or CI runner, not on a machine anyone depends on.

## Status

| # | Milestone | Status |
|---|-----------|--------|
| 0 | Skeleton: Go module, `minibox` CLI, the Lima VM, tests in the VM as root, CI, a pinned test root filesystem | Built |
| 1 | Isolation: namespaces, `pivot_root`, `/proc` and `/dev`, an init as PID 1 | Built |
| 2 | Images: pulled from a registry and checked, safe layer extraction, overlayfs | Built |
| 3 | Limits: cgroup v2 memory, CPU and pids; OOM kills reported | Not started |
| 4 | Networking: veth pairs on a bridge, NAT and published ports, netlink by hand | Not started |
| 5 | Security: capabilities, `no_new_privs`, seccomp, masked paths, user namespaces | Not started |
| 6 | The OCI runtime spec (a subset), compared with runc; start-up time measured | Not started |
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
cgroup v2 with the cpu, memory and pids controllers, overlayfs, and, for
later milestones, user namespaces and nftables. Exit 0 when everything
needed is there, 1 when something is missing, 2 for a usage error.

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
after it exits). The exit code is the command's, or 128 + the signal that
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

## How a container starts

```
minibox run ──► Run: checks the config, locks its OS thread, starts /proc/self/exe init
   with CLONE_NEWUTS|NEWPID|NEWNS|NEWIPC, a session of its own, and Pdeathsig=SIGKILL
   ──► sends the config as JSON on fd 3 ──► waits for one status message on fd 4
minibox init (PID 1 in the new namespaces) ──► / made a private mount, so nothing travels back
   ──► the root filesystem bind-mounted on itself, then pivot_root(".", ".") and the old root
       detached: no path leads back to the host's files
   ──► /proc, a /dev of its own, /sys read-only, the hostname ──► the command started
       (found in the container's PATH) ──► "started" on fd 4
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
| Namespaces | its own UTS (hostname), PID, mount and IPC; the host's network, users and cgroups (Milestones 3-5) |
| `/` | the root filesystem directory, through `pivot_root`, with the old root detached |
| `/proc` | its own (only its processes), `nosuid,nodev,noexec` |
| `/dev` | a tmpfs with `null`, `zero`, `full`, `random`, `urandom`, `tty`, `fd`, `stdin`/`stdout`/`stderr`, `pts` (a new devpts instance), `ptmx`, `shm`, `mqueue`; none of the host's disks |
| `/sys` | read-only |
| Files open | 0, 1 and 2 only: Init's pipes to `minibox run` are closed on exec |

A top-level `/proc`, `/dev` or `/sys` that is a symbolic link or a file is
refused. The special filesystems are mounted after `pivot_root`, so any
link in the root filesystem could only point at something else inside it.

**Not yet:** the command runs as root with every capability (it could
mount filesystems, load kernel modules or reboot the VM: Milestone 5),
shares the host's network (and `/sys` shows the host's network devices:
Milestone 4), has no resource limits (Milestone 3) and no terminal of its
own (`-t`): an interactive shell works, without job control.

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
- **The command end to end** (`cmd/minibox`), with the test binary playing
  `minibox`: the default hostname is the id, usage errors are 125, Ctrl-C
  sent to `minibox run` ends the container with 130, and killing `minibox
  run` with `SIGKILL` takes the container with it.
- **44 planted bugs** each fail the tests: 5 in Milestone 0 (failures not
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
  the working directory not made). Five were caught only after a fix
  (failures 1-3).
- **The smoke test** (`make smoke`, 23 checks): a static Linux binary,
  `check` passing as root in the VM with no warnings, failing as an
  ordinary user and saying why, failing on the Mac and saying Linux is
  needed, usage errors exiting 2; real containers on a folder: their own
  hostname, Init as PID 1 with no host process visible, exit codes 7 and
  127 passed back, 125 without root, nothing left running or mounted; and
  a real image: `minibox pull alpine:3.22` from Docker Hub, `images`,
  Alpine 3.22 inside, a write that stays in its container, no container
  folder or overlay left, `rmi` removing the layers, 125 for an image that
  is not there.
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

## Layout

| Path | What |
|------|------|
| `cmd/minibox/` | The command line and its exit codes; end-to-end tests of `run` |
| `internal/container/config.go`, `lookpath.go` | A container's configuration and environment; finding the command in the container's PATH |
| `internal/container/run_linux.go` | `minibox run`'s side: starting Init in new namespaces, the config and status pipes, passing signals on, exit codes |
| `internal/container/init_linux.go` | Init, PID 1: setting up, starting the command, reaping, forwarding signals |
| `internal/container/rootfs_linux.go` | Private mounts, `pivot_root`, `/proc`, `/dev`, `/sys` |
| `internal/image/reference.go`, `oci.go` | Image names as Docker reads them; OCI and Docker manifest types; choosing this machine's platform |
| `internal/image/registry.go` | The registry client: bearer tokens, manifests and blobs, every digest checked |
| `internal/image/extract_linux.go` | Unpacking a layer safely, with whiteouts as overlayfs reads them |
| `internal/image/store.go` | The store: pull, list, remove, a container's overlay folders |
| `internal/container/user.go` | Users and groups from the container's own `/etc/passwd` and `/etc/group` |
| `internal/testutil/` | Root required (or a skip), the test root filesystem, the escape probe, a fake registry and layer builders |
| `scripts/check_extraction/` | Unpacking real images by MiniBox and by GNU tar, compared |
| `internal/host/` | `minibox check`: what the machine has, checked one item at a time |
| `lima/minibox.yaml` | The development VM |
| `scripts/vm-test.sh` | The checks, in the VM, as root |
| `scripts/rootfs.sh` | The pinned test root filesystem |
| `scripts/smoke.sh` | The binary, checked from outside |
| `.github/workflows/ci.yml` | CI on Ubuntu 24.04 (written; it has not run) |
