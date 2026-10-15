# Explain it yourself

Questions to answer out loud, without notes, before I talk about MiniBox
in an interview. There are no answers here: the explanation has to be
mine. Two things are true at once, and I should say both plainly.

- MiniBox is **Go**, which is not yet on my language list, and it was
  **built with Claude** — like Gatekeeper. So being able to explain it
  does *not* lift the Go-ramp gate: Go stays off my resume and skills
  list until I pass that gate (`projects/go-ramp/GATE.md`) on my own.
- Even so, if I am going to show MiniBox or talk about it, the bar is the
  same as for any project: I have to be able to say *why* it is built this
  way and *what went wrong*, not just what the code does. These questions
  are about that.

## Run it first

1. `make vm` then `make vm-test`: watch it copy the source into
   `~/.cache/minibox-dev` and run the checks as root in the VM. Why is the
   project folder itself never shared with the VM, and what breaks if I
   share the iCloud folder instead?
2. In the VM: `minibox pull alpine:3.22`, then
   `minibox run alpine:3.22 sh -c 'hostname; id; ps'`. Point at each line
   of output and name the namespace or setup step that produced it. Why is
   the only process besides the shell `minibox-init` as PID 1?
3. `minibox run --rootfs /var/tmp/alpine -- /bin/sh -c 'cat /proc/self/cgroup; head -c1 /proc/kcore | wc -c'`.
   Why does `/proc/self/cgroup` read `0::/`? Why does `/proc/kcore` read
   empty when `/proc/sys` still reads?
4. Make a bundle (`runc spec` writes a `config.json`), then
   `minibox create --bundle . c1`, `minibox state c1`, `minibox start c1`,
   `minibox state c1`. Explain each state, and say exactly what is and is
   not running after `create` but before `start`.
5. `make smoke` (51 checks). Pick five — include the escape probe, "no
   overlay is left on the host", and the `--userns` uid check — and say
   what each one proves and what it would catch if MiniBox misbehaved.

## Isolation

6. Why does entering new namespaces need a second process ("re-exec")
   instead of the running program unsharing itself? What is special about
   the Go runtime here?
7. Why is Init PID 1 and not the command, in `minibox run`? Name the two
   things the kernel does only for PID 1. Why is there exactly one waiter,
   and what goes wrong with two?
8. Which signals does Init forward to the command, and which does it
   deliberately not? Why not `SIGCHLD`, and why does the Go runtime's
   `SIGURG` have to be left alone?
9. `pivot_root` with the old root detached, versus `chroot`: what can a
   process do after `chroot` that it cannot after `pivot_root`? How does
   the probe demonstrate the difference?
10. Why are `/proc`, `/dev` and `/sys` mounted into the rootfs *before*
    `pivot_root`, not after? Tell failure 16b as a rule about user
    namespaces and a fully visible procfs.
11. `Pdeathsig` kills Init if `minibox run` dies — but in Go this needs
    the starting OS thread locked for the whole life of the container. Why
    the thread and not the process? And what is the `getppid() == 0` side
    effect in a new PID namespace that `strace` shows?

## Images

12. Walk a `minibox pull` from the name `alpine:3.22` to layers on disk.
    Where does the bearer token come from, how many requests does it
    cover, and which digests are checked against what?
13. A layer is unpacked while it is still downloading. Why keep nothing
    compressed on disk, and why must the unpacking be safe for a layer
    that *turns out* to fail its digest?
14. How do `openat2(RESOLVE_IN_ROOT)` and the `*at` syscalls stop a layer
    from writing outside its folder? Why is a bare `a/../b` refused when
    Docker accepts it?
15. Why are a layer's own `trusted.*` xattrs never applied, while file
    capabilities and `user.*` are? What could a layer forge with
    `trusted.overlay.*`?
16. The overlay is mounted inside the container's mount namespace. Why
    does that mean no overlay is ever visible on the host, even if
    `minibox run` is killed? How is that tested, during and after?

## Limits

17. Why cgroup v2 only, and why is the group made before the container
    exists and the container born into it with `CLONE_INTO_CGROUP`? What
    window does that close?
18. Why is swap off by default under `--memory`? What would `--memory 64m`
    without that look like, and why would it not really be a limit?
19. The container reads its own limits at `/sys/fs/cgroup` but cannot
    change them. What does the cgroup namespace plus a read-only mount buy
    you here?
20. Failure 7: not enabling the controllers in MiniBox's parent group
    passed every test at first. Why did it pass, and what does the fix —
    a fresh parent group per test — prove?

## Networking

21. Route netlink is written by hand, but nftables goes through the `nft`
    command. Why that split, and why is it honest to say so out loud? What
    would writing the nftables rules by hand have added?
22. How is an address leased, and why is a holder recognised by pid *and*
    start time rather than pid alone (failure 11)?
23. Failure 10: a container could not reach another container's published
    port through the host's address. Trace the packet and say why it was
    dropped; what does hairpin masquerade change?
24. Why is `CAP_NET_RAW` dropped, what stops working because of it, and
    how do the tests check reachability without `ping`?
25. Nothing about networking was measured. Why is that the honest line,
    and what would it take to make a throughput claim defensible?

## Security

26. MiniBox keeps Docker's default capabilities minus `CAP_NET_RAW` and
    `CAP_MKNOD`. Why those two specifically? What could a container do
    with `CAP_MKNOD` that the missing devices cgroup makes dangerous?
27. A non-root `-u` command gets zero capabilities. Why is that correct
    and not a bug, and how does it match Docker?
28. Failure 15: the seccomp BPF was rejected with `EINVAL`. Explain the
    jump-offset bug — why did counting `len(filter) + i` point the jumps
    past the end of the program? What is the right landing index?
29. `keyctl` needs no capability. Why does denying it prove the seccomp
    filter is load-bearing in a way the capability drop alone is not?
30. `--userns` maps the container's root to an unprivileged host uid, and
    a `Credential` on the clone makes Init the namespace root before the
    exec. Why is that `Credential` needed (failure 16a)? What is still not
    mapped, and why is running an image under `--userns` refused?

## The OCI lifecycle

31. Why does `create` block on an exec fifo, and why is the fifo opened
    `O_RDWR` and not `O_RDONLY` (failure 18)?
32. The OCI path `execve`s the command so it is PID 1; `run` keeps Init as
    PID 1 and supervises. What does each model buy, and why does the OCI
    path need its own `chdir` that the `run` path does not (failure 19)?
33. It is a subset: the config's capabilities, mounts and seccomp are
    ignored and MiniBox applies its own. Why is the runc comparison still
    meaningful, and what exactly does "8 of 8 matched" claim — and not
    claim?

## Testing and measuring

34. Why do the tests run with `MINIBOX_TEST_REQUIRE=1`, which turns a skip
    into a failure? What class of mistake does that catch?
35. 118 planted bugs, and several passed at first (failures 1-3, 7, 14).
    Pick one and explain how a passing planted bug is a bug in the *test*,
    not the code. Why are two of Milestone 6's caught only by the
    five-minute suite timeout?
36. The start-up benchmark says MiniBox's `create` is faster than runc's.
    Why is that true, and why is it *not* a claim that MiniBox is faster
    or better than runc?

## The two that matter in an interview

37. One design decision I would defend, in my own words, and the cost I
    accepted for it.
38. One failure mode I found, told as a story: the symptom, the cause, the
    fix, and how I know it is fixed now.
