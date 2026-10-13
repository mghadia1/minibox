//go:build linux

package container

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mghadia1/minibox/internal/network"
	"github.com/mghadia1/minibox/internal/testutil"
)

// background runs a container until its stdin closes; it writes its eth0
// address to /ip in its root filesystem first.
type background struct {
	rootfs string
	stdin  *io.PipeWriter
	done   chan result
}

func startBackground(t *testing.T, cfg Config, script string) *background {
	t.Helper()
	r, w := io.Pipe()
	b := &background{rootfs: cfg.Rootfs, stdin: w, done: make(chan result, 1)}
	cfg.Args = []string{"/bin/sh", "-c", "ip -4 -o addr show eth0 2>/dev/null | awk '{print $4}' | cut -d/ -f1 > /ip; touch /ready; " + script}
	go func() {
		var stdout, stderr bytes.Buffer
		res, err := Run(cfg, Stdio{In: r, Out: &stdout, Err: &stderr}, nil)
		b.done <- result{stdout.String(), stderr.String(), res.Code, err}
	}()
	waitFor(t, filepath.Join(cfg.Rootfs, "ready"))
	t.Cleanup(func() { b.stop(t) })
	return b
}

func (b *background) ip(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(b.rootfs, "ip"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func (b *background) stop(t *testing.T) result {
	t.Helper()
	b.stdin.Close()
	select {
	case r := <-b.done:
		b.done <- r
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("a background container did not stop")
		return result{}
	}
}

func httpGet(url string) (string, error) {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func TestBridgeNetwork(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	// No ping: CAP_NET_RAW is dropped (Milestone 5), so ICMP is not
	// available. Real reachability is covered by TestContainersReachEachOther
	// and TestTrafficOutIsMasqueraded, over TCP. Here, eth0 up with an
	// address and a default route is what the setup promises.
	out := mustSucceed(t, sh(t, rootfs, "ip -4 -o addr show eth0 | awk '{print $4}'; ip route | head -1; tail -1 /etc/hosts; grep -c '^nameserver 127' /etc/resolv.conf; ip -o link show eth0 | grep -q ',UP' && echo eth0-up"))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 {
		t.Fatalf("output %q", out)
	}
	addr, route, hosts, loopbackDNS, up := lines[0], lines[1], lines[2], lines[3], lines[4]
	ip, ipnet, err := net.ParseCIDR(addr)
	if err != nil || !ipnet.Contains(net.ParseIP("10.99.0.1")) || ip.Equal(net.ParseIP("10.99.0.1")) {
		t.Errorf("eth0 has %q, want an address in 10.99.0.0/24 other than the bridge's", addr)
	}
	if !strings.HasPrefix(route, "default via 10.99.0.1 dev eth0") {
		t.Errorf("route %q", route)
	}
	if hosts != ip.String()+"\tbox" {
		t.Errorf("/etc/hosts ends %q", hosts)
	}
	if loopbackDNS != "0" {
		t.Errorf("/etc/resolv.conf names a loopback nameserver")
	}
	if up != "eth0-up" {
		t.Errorf("eth0 is not up")
	}
}

func TestContainersReachEachOther(t *testing.T) {
	testutil.RequireRoot(t)
	a := testutil.Rootfs(t)
	testutil.Probe(t, a)
	cfgA := config(a)
	cfgA.ID, cfgA.Hostname = NewID(), "server"
	server := startBackground(t, cfgA, "/probe serve 8000 & read line")
	ip := server.ip(t)
	b := testutil.Rootfs(t)
	// The server may need a moment to listen.
	out := mustSucceed(t, sh(t, b, fmt.Sprintf("for i in 1 2 3 4 5 6 7 8 9 10; do wget -q -T 2 -O - http://%s:8000/ && exit 0; sleep 0.2; done; exit 1", ip)))
	if out != "hello from server" {
		t.Fatalf("got %q", out)
	}
}

func TestPublishedPort(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	testutil.Probe(t, rootfs)
	cfg := config(rootfs)
	cfg.ID, cfg.Hostname = NewID(), "published"
	cfg.Ports = []network.Port{{Proto: "tcp", Host: 18080, Container: 8000}}
	server := startBackground(t, cfg, "/probe serve 8000 & read line")
	hostIP := primaryIP(t)
	for _, url := range []string{"http://127.0.0.1:18080/", "http://" + hostIP + ":18080/"} {
		var got string
		var err error
		for range 20 {
			if got, err = httpGet(url); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if got != "hello from published" {
			t.Errorf("%s: %q, %v", url, got, err)
		}
	}
	// From another machine, through the host's prerouting chain: the
	// outside namespace connects to the host's address on its link.
	ns, hostAddr, _ := outsideNamespace(t)
	out, err := exec.Command("ip", "netns", "exec", ns, filepath.Join(rootfs, "probe"), "get", "http://"+hostAddr+":18080/").CombinedOutput()
	if err != nil || string(out) != "hello from published" {
		t.Errorf("from another machine: %q, %v", out, err)
	}
	// From another container, through the host's address on the bridge:
	// the request goes out to the bridge it came from.
	other := testutil.Rootfs(t)
	testutil.Probe(t, other)
	if r := runConfig(t, config(other, "/probe", "get", "http://"+network.Gateway().String()+":18080/"), "", nil); r.stdout != "hello from published" {
		t.Errorf("from another container: %q, exit %d, %v %s", r.stdout, r.code, r.err, r.stderr)
	}
	// A second container cannot publish the same port.
	cfg2 := config(other, "/bin/true")
	cfg2.Ports = cfg.Ports
	if r := runConfig(t, cfg2, "", nil); r.code != ExitSetup || !strings.Contains(fmt.Sprint(r.err), "already published") {
		t.Errorf("the same port twice: exit %d, %v", r.code, r.err)
	}
	server.stop(t)
	if _, err := httpGet("http://127.0.0.1:18080/"); err == nil {
		t.Error("the port still answers after the container stopped")
	}
	if out, _ := exec.Command("nft", "list", "map", "ip", "minibox", "ports").CombinedOutput(); strings.Contains(string(out), "18080") {
		t.Errorf("the port is still in the map:\n%s", out)
	}
}

// primaryIP is one of the host's own addresses that is not loopback and
// not the bridge's.
func primaryIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() && !strings.HasPrefix(ipnet.IP.String(), "10.99.0.") {
			return ipnet.IP.String()
		}
	}
	t.Fatal("no address to test with")
	return ""
}

func TestNoNetwork(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	// loopback is checked by its link state, not ping: CAP_NET_RAW is
	// dropped (Milestone 5).
	cfg := config(rootfs, "/bin/sh", "-c", "ls /sys/class/net; ip -o link | wc -l; tail -1 /etc/hosts; ip -o link show lo | grep -q ',UP' && echo loopback-up")
	cfg.NetworkMode = network.None
	out := strings.Split(strings.TrimSpace(mustSucceed(t, runConfig(t, cfg, "", nil))), "\n")
	if len(out) != 4 || out[0] != "lo" || strings.TrimSpace(out[1]) != "1" || out[2] != "127.0.1.1\tbox" || out[3] != "loopback-up" {
		t.Fatalf("with --network none: %q", out)
	}
}

func TestHostNetwork(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	cfg := config(rootfs, "readlink", "/proc/self/ns/net")
	cfg.NetworkMode = network.Host
	host, _ := os.Readlink("/proc/self/ns/net")
	if got := strings.TrimSpace(mustSucceed(t, runConfig(t, cfg, "", nil))); got != host {
		t.Fatalf("--network host: the container's network namespace is %s, the host's %s", got, host)
	}
	cfg.Ports = []network.Port{{Proto: "tcp", Host: 18081, Container: 80}}
	if r := runConfig(t, cfg, "", nil); r.code != ExitSetup {
		t.Fatalf("a port on the host network: exit %d", r.code)
	}
}

func readLeases(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(network.StateDir, "leases.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var leases map[string]json.RawMessage
	if err := json.Unmarshal(data, &leases); err != nil {
		t.Fatal(err)
	}
	return leases
}

// Containers started at the same time get different addresses, and give
// them back, with their veths, when they end.
func TestAddressesAreUniqueAndReturned(t *testing.T) {
	testutil.RequireRoot(t)
	const n = 8
	var wg sync.WaitGroup
	ips := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		rootfs := testutil.Rootfs(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := sh(t, rootfs, "ip -4 -o addr show eth0 | awk '{print $4}' | cut -d/ -f1; sleep 1")
			ips[i], errs[i] = strings.TrimSpace(r.stdout), r.err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("container %d: %v", i, err)
		}
	}
	sorted := slices.Clone(ips)
	slices.Sort(sorted)
	if len(slices.Compact(sorted)) != n || slices.Contains(sorted, "") {
		t.Fatalf("addresses %q are not %d different ones", ips, n)
	}
	leases := readLeases(t)
	for _, ip := range ips {
		// As the leases name them: "10.99.0.2", not ip's "10.99.0.2/24"
		// (which once made this check unable to fail).
		if net.ParseIP(ip) == nil {
			t.Fatalf("%q is not an address", ip)
		}
		if _, held := leases[ip]; held {
			t.Errorf("%s is still leased after its container ended", ip)
		}
	}
	out, _ := exec.Command("ip", "-o", "link").CombinedOutput()
	if strings.Contains(string(out), " mbx") && !otherTestsRunning() {
		t.Errorf("veths left on the host:\n%s", out)
	}
}

// otherTestsRunning: other packages' containers may hold leases and veths
// of their own at the same time.
func otherTestsRunning() bool {
	entries, _ := os.ReadDir("/proc")
	me := fmt.Sprint(os.Getpid())
	for _, e := range entries {
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err == nil && bytes.HasPrefix(cmdline, []byte("minibox-init\x00")) {
			stat, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
			if f := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:])); len(f) > 1 && f[1] != me {
				return true
			}
		}
	}
	return false
}

// lockLeases takes the lease file's lock, as MiniBox does: other
// packages' containers may be starting at the same time.
func lockLeases(t *testing.T) func() {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(network.StateDir, "network.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() { f.Close() }
}

// A lease whose `minibox run` died without giving it back, and the port
// it published, are cleared by the next container to start; so is one
// whose owner's pid now belongs to a different process.
func TestStaleLeasesAreReclaimed(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	mustSucceed(t, sh(t, rootfs, "true")) // the bridge and the table exist
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	unlock := lockLeases(t)
	leases := readLeases(t)
	if leases == nil {
		leases = map[string]json.RawMessage{}
	}
	leases["10.99.0.250"] = json.RawMessage(fmt.Sprintf(`{"id":"stale","owner":%d,"started":1,"ports":[{"proto":"tcp","host":18099,"container":80}]}`, dead.Process.Pid))
	// This test's own pid is alive, but did not start at clock tick 1.
	leases["10.99.0.249"] = json.RawMessage(fmt.Sprintf(`{"id":"reused","owner":%d,"started":1}`, os.Getpid()))
	data, _ := json.Marshal(leases)
	if err := os.WriteFile(filepath.Join(network.StateDir, "leases.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("nft", "add", "element", "ip", "minibox", "ports", "{ tcp . 18099 : 10.99.0.250 . 80 }").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	unlock()
	mustSucceed(t, sh(t, rootfs, "true"))
	now := readLeases(t)
	if _, ok := now["10.99.0.250"]; ok {
		t.Error("the stale lease is still there")
	}
	if _, ok := now["10.99.0.249"]; ok {
		t.Error("a lease whose owner's pid was reused is still there")
	}
	if out, _ := exec.Command("nft", "list", "map", "ip", "minibox", "ports").CombinedOutput(); strings.Contains(string(out), "18099") {
		t.Errorf("the stale port is still published:\n%s", out)
	}
}

// outsideNamespace makes a network namespace standing for another
// machine on the host's network: 192.0.2.0/24 (a documentation range) on
// a veth to the host, the host 192.0.2.2 and the namespace 192.0.2.1, and
// no route to the bridge's addresses. A container reaches it only if the
// host masquerades its traffic; it reaches a container only through a
// port the host publishes.
func outsideNamespace(t *testing.T) (ns, hostAddr, outsideAddr string) {
	t.Helper()
	cmds := [][]string{
		{"ip", "netns", "add", "mbtest-outside"},
		{"ip", "link", "add", "mbto-host", "type", "veth", "peer", "name", "mbto-out", "netns", "mbtest-outside"},
		{"ip", "addr", "add", "192.0.2.2/24", "dev", "mbto-host"},
		{"ip", "link", "set", "mbto-host", "up"},
		{"ip", "-n", "mbtest-outside", "addr", "add", "192.0.2.1/24", "dev", "mbto-out"},
		{"ip", "-n", "mbtest-outside", "link", "set", "mbto-out", "up"},
		{"ip", "-n", "mbtest-outside", "link", "set", "lo", "up"},
	}
	t.Cleanup(func() {
		_ = exec.Command("ip", "link", "del", "mbto-host").Run()
		_ = exec.Command("ip", "netns", "del", "mbtest-outside").Run()
	})
	for _, c := range cmds {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%q: %v: %s", c, err, out)
		}
	}
	return "mbtest-outside", "192.0.2.2", "192.0.2.1"
}

func TestTrafficOutIsMasqueraded(t *testing.T) {
	testutil.RequireRoot(t)
	rootfs := testutil.Rootfs(t)
	testutil.Probe(t, rootfs)
	ns, _, addr := outsideNamespace(t)
	server := exec.Command("ip", "netns", "exec", ns, filepath.Join(rootfs, "probe"), "serve", "8000")
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
	host, _ := os.Hostname()
	out := mustSucceed(t, sh(t, rootfs, fmt.Sprintf("for i in 1 2 3 4 5 6 7 8 9 10; do wget -q -T 2 -O - http://%s:8000/ && exit 0; sleep 0.2; done; exit 1", addr)))
	if out != "hello from "+host {
		t.Fatalf("got %q", out)
	}
}
