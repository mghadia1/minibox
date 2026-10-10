# MiniBox

A container runtime built from the Linux kernel's own pieces: namespaces,
`pivot_root`, cgroup v2, overlayfs, virtual network devices, capabilities
and seccomp. The point is to know what Docker and runc do underneath, so
none of runc, containerd, libcontainer, go-containerregistry, CNI plugins,
netlink libraries or libseccomp is used; `golang.org/x/sys/unix` (system
call wrappers) is allowed, and runc may appear in tests as a reference.

> **Status:** Milestone 0 of 7 built, on my machine (no GitHub repository
> yet, so CI has not run): the `minibox` command checks whether a machine
> can run containers, and the Linux VM it is developed in is defined in
> this repository. Containers come in Milestone 1.

> **Where it runs:** Linux only, as root. On my Mac that is a Lima VM
> (`lima/minibox.yaml`: Ubuntu 24.04, 2 CPUs, 2 GiB, 10 GiB disk); in CI,
> GitHub's Ubuntu runners. From Milestone 4 MiniBox changes the network
> setup of the machine it runs on (a bridge, nftables rules), so it belongs
> in a disposable VM or CI runner, not on a machine anyone depends on.

## Status

| # | Milestone | Status |
|---|-----------|--------|
| 0 | Skeleton: Go module, `minibox` CLI, the Lima VM, tests in the VM as root, CI, a pinned test root filesystem | Built |
| 1 | Isolation: namespaces, `pivot_root`, `/proc` and `/dev`, an init as PID 1 | Not started |
| 2 | Images: pulled from a registry and checked, safe layer extraction, overlayfs | Not started |
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
make smoke       # the binary from outside: in the VM as root and not, and on the Mac
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
- **5 planted bugs** each fail the tests (failures not counted, filesystem
  names read from the wrong field, missing items inverted, cgroup v1
  accepted, `check` exiting 0 on a failure). The last passed at first: in
  the VM nothing fails, so the exit-code test only ever saw success; it now
  runs `check` against reports that fail and that only warn.
- **The smoke test** (`make smoke`, 8 checks): a static Linux binary,
  `check` passing as root in the VM with no warnings, failing as an
  ordinary user and saying why, failing on the Mac and saying Linux is
  needed, usage errors exiting 2.

## Layout

| Path | What |
|------|------|
| `cmd/minibox/` | The command line and its exit codes |
| `internal/host/` | `minibox check`: what the machine has, checked one item at a time |
| `lima/minibox.yaml` | The development VM |
| `scripts/vm-test.sh` | The checks, in the VM, as root |
| `scripts/rootfs.sh` | The pinned test root filesystem |
| `scripts/smoke.sh` | The binary, checked from outside |
| `.github/workflows/ci.yml` | CI on Ubuntu 24.04 (written; it has not run) |
