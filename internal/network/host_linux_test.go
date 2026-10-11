//go:build linux

package network

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mghadia1/minibox/internal/testutil"
)

// Attach and Detach on their own, for processes in network namespaces of
// their own: no container needed.

// inNamespace starts a process in a new network namespace, for Attach to
// wire up, and returns its pid.
func inNamespace(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// newID is a container id unique in its first 11 characters (the veth's
// name is made from them).
func newID() string { return fmt.Sprintf("%012x", rand.Uint64()&(1<<48-1)) }

func leased(t *testing.T, ip net.IP) bool {
	t.Helper()
	unlock, err := lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	leases, err := readLeases()
	if err != nil {
		t.Fatal(err)
	}
	_, ok := leases[ip.String()]
	return ok
}

// Attaches at the same moment get different addresses only because of the
// lock: the hook holds each one between reading the leases and writing
// them, so without the lock all four would read the same leases.
func TestConcurrentAttachesGetDifferentAddresses(t *testing.T) {
	testutil.RequireRoot(t)
	testHookLeasesRead = func() { time.Sleep(200 * time.Millisecond) }
	defer func() { testHookLeasesRead = nil }()
	const n = 4
	atts := make([]*Attachment, n)
	errs := make([]error, n)
	pids := make([]int, n)
	for i := range n {
		pids[i] = inNamespace(t)
	}
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			atts[i], errs[i] = Attach(newID(), pids[i], nil)
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, a := range atts {
		if errs[i] != nil {
			t.Fatalf("attach %d: %v", i, errs[i])
		}
		defer a.Detach()
		if seen[a.IP.String()] {
			t.Errorf("%s was given out twice", a.IP)
		}
		seen[a.IP.String()] = true
	}
}

// Detach gives back everything Attach took: the lease, the host's veth and
// the published port.
func TestDetachGivesEverythingBack(t *testing.T) {
	testutil.RequireRoot(t)
	a, err := Attach(newID(), inNamespace(t), []Port{{Proto: "tcp", Host: 18087, Container: 80}})
	if err != nil {
		t.Fatal(err)
	}
	veth := filepath.Join("/sys/class/net", a.HostVeth)
	if _, err := os.Stat(veth); err != nil || !leased(t, a.IP) {
		t.Fatalf("attached: veth %v, leased %v", err, leased(t, a.IP))
	}
	if err := a.Detach(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(veth); err == nil {
		t.Error("the host's veth is still there")
	}
	if leased(t, a.IP) {
		t.Error("the address is still leased")
	}
	if out, _ := exec.Command("nft", "list", "map", "ip", "minibox", "ports").CombinedOutput(); strings.Contains(string(out), "18087") {
		t.Errorf("the port is still published:\n%s", out)
	}
}

// A port a program on the host already holds is refused, TCP and UDP;
// once the program lets go, it can be published.
func TestPortInUseOnTheHostIsRefused(t *testing.T) {
	testutil.RequireRoot(t)
	l, err := net.Listen("tcp", "127.0.0.1:18088")
	if err != nil {
		t.Fatal(err)
	}
	u, err := net.ListenPacket("udp", ":18089")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Port{{Proto: "tcp", Host: 18088, Container: 80}, {Proto: "udp", Host: 18089, Container: 53}} {
		if a, err := Attach(newID(), inNamespace(t), []Port{p}); err == nil || !strings.Contains(err.Error(), "in use on the host") {
			if a != nil {
				a.Detach()
			}
			t.Errorf("%d/%s, held by this test: %v", p.Host, p.Proto, err)
		}
	}
	l.Close()
	u.Close()
	a, err := Attach(newID(), inNamespace(t), []Port{{Proto: "tcp", Host: 18088, Container: 80}, {Proto: "udp", Host: 18089, Container: 53}})
	if err != nil {
		t.Fatalf("once let go: %v", err)
	}
	if err := a.Detach(); err != nil {
		t.Fatal(err)
	}
}
