//go:build linux

package network

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mghadia1/minibox/internal/netlink"
)

// StateDir holds the address leases. /run is a tmpfs: leases do not
// outlive a reboot, and neither do containers.
var StateDir = "/run/minibox"

// The nftables table MiniBox owns: NAT out through the host for the
// bridge's addresses, published ports as a map from (protocol, host port)
// to (container address, container port), and forwarding to and from the
// bridge allowed. Masquerading in postrouting, in order: a container's
// traffic leaving the host; the host's own 127.0.0.1 reaching a published
// port (a container could not answer 127.0.0.1); and a container reaching
// a published port through the host's address, which DNAT sends back to
// the bridge it came from (without it, the answer would go straight back
// across the bridge from an address the asker never wrote to). Applied
// once, when the table is missing: changing it means deleting the table
// while no container runs.
const ruleset = `table ip minibox {
	map ports {
		type inet_proto . inet_service : ipv4_addr . inet_service
	}
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		fib daddr type local dnat ip to meta l4proto . th dport map @ports
	}
	chain output {
		type nat hook output priority dstnat; policy accept;
		fib daddr type local dnat ip to meta l4proto . th dport map @ports
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr ` + subnetCIDR + ` oifname != "` + BridgeName + `" masquerade
		ip saddr 127.0.0.0/8 oifname "` + BridgeName + `" masquerade
		ip saddr ` + subnetCIDR + ` oifname "` + BridgeName + `" ct status dnat masquerade
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "` + BridgeName + `" accept
		oifname "` + BridgeName + `" accept
	}
}
`

type lease struct {
	ID    string `json:"id"`
	Owner int    `json:"owner"` // the pid of the `minibox run` holding it
	// When the owner started (see startTime): a pid alone could be a new
	// process that was given a dead owner's number.
	Started uint64 `json:"started"`
	Ports   []Port `json:"ports,omitempty"`
}

// Attachment is a container's place on the bridge, until Detach.
type Attachment struct {
	ID       string
	IP       net.IP
	HostVeth string
	Ports    []Port
}

// testHookLeasesRead, when a test sets it, runs between reading the
// leases and writing them back: it widens the window in which two
// `minibox run`s without the lock would take the same address.
var testHookLeasesRead func()

// inUse refuses a port that a socket on the host already holds: a TCP
// listener or a bound UDP socket, on any of the host's addresses. DNAT
// comes before a socket is looked up, so publishing that port would take
// its traffic away from it (publishing 22 would cut off the VM's SSH).
// Docker's proxy would fail to bind it instead.
func inUse(ports []Port) error {
	state := map[string]string{"tcp": "0A", "udp": "07"}
	for _, p := range ports {
		for _, table := range []string{p.Proto, p.Proto + "6"} {
			data, err := os.ReadFile("/proc/net/" + table)
			if err != nil {
				continue // no IPv6
			}
			if boundPorts(string(data), state[p.Proto])[p.Host] {
				return fmt.Errorf("port %d/%s is in use on the host: publishing it would take that program's traffic", p.Host, p.Proto)
			}
		}
	}
	return nil
}

// HostVethName is the host's end of a container's veth pair. Interface
// names are at most 15 characters.
func HostVethName(id string) string {
	if len(id) > 11 {
		id = id[:11]
	}
	return "mbx" + id
}

// lock takes the lease file's lock: attaching and detaching are one at a
// time across every `minibox run`.
func lock() (func(), error) {
	if err := os.MkdirAll(StateDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(StateDir, "network.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func leasesPath() string { return filepath.Join(StateDir, "leases.json") }

func readLeases() (map[string]lease, error) {
	data, err := os.ReadFile(leasesPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]lease{}, nil
	}
	if err != nil {
		return nil, err
	}
	leases := map[string]lease{}
	if err := json.Unmarshal(data, &leases); err != nil {
		return nil, fmt.Errorf("%s: %w", leasesPath(), err)
	}
	return leases, nil
}

func writeLeases(leases map[string]lease) error {
	data, err := json.MarshalIndent(leases, "", "  ")
	if err != nil {
		return err
	}
	tmp := leasesPath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, leasesPath())
}

// startTime is when a process started, in clock ticks after boot (field
// 22 of /proc/PID/stat), or 0 if there is no such process.
func startTime(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// Field 2, the command name, is in parentheses and may hold spaces.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(string(data[i+1:]))
	if len(f) < 20 {
		return 0
	}
	n, _ := strconv.ParseUint(f[19], 10, 64) // f[0] is field 3
	return n
}

// alive reports whether a lease's owner is still running: the same pid,
// started at the same time.
func (l lease) alive() bool {
	return l.Owner > 0 && l.Started != 0 && startTime(l.Owner) == l.Started
}

// Attach puts a container on the bridge: an address leased to it, a veth
// pair with eth0 in the network namespace of its init (pid), the host end
// on the bridge, and its published ports in the nftables map. It is
// called by Run once Init exists, before Init is told to go on.
func Attach(id string, pid int, ports []Port) (*Attachment, error) {
	unlock, err := lock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	leases, err := readLeases()
	if err != nil {
		return nil, err
	}
	if testHookLeasesRead != nil {
		testHookLeasesRead()
	}
	// Leases of a `minibox run` that died without detaching: their veth
	// went with the container's namespace; their ports must go too.
	for ip, l := range leases {
		if !l.alive() {
			removePorts(net.ParseIP(ip), l.Ports)
			delete(leases, ip)
		}
	}
	for _, l := range leases {
		for _, p := range l.Ports {
			for _, q := range ports {
				if p.Proto == q.Proto && p.Host == q.Host {
					return nil, fmt.Errorf("port %d/%s is already published by container %s", q.Host, q.Proto, l.ID)
				}
			}
		}
	}
	if err := inUse(ports); err != nil {
		return nil, err
	}
	ip, err := freeAddress(leases)
	if err != nil {
		return nil, err
	}
	if err := ensureBridge(); err != nil {
		return nil, err
	}

	s, err := netlink.Open()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	a := &Attachment{ID: id, IP: ip, HostVeth: HostVethName(id), Ports: ports}
	if err := s.CreateVeth(a.HostVeth, "eth0", pid); err != nil {
		return nil, err
	}
	fail := func(err error) (*Attachment, error) {
		if idx, e := s.LinkIndex(a.HostVeth); e == nil {
			_ = s.DeleteLink(idx)
		}
		return nil, err
	}
	host, err := s.LinkIndex(a.HostVeth)
	if err != nil {
		return fail(err)
	}
	bridge, err := s.LinkIndex(BridgeName)
	if err != nil {
		return fail(err)
	}
	if err := s.SetMaster(host, bridge); err != nil {
		return fail(err)
	}
	if err := s.SetUp(host); err != nil {
		return fail(err)
	}
	if err := addPorts(ip, ports); err != nil {
		removePorts(ip, ports)
		return fail(err)
	}
	leases[ip.String()] = lease{ID: id, Owner: os.Getpid(), Started: startTime(os.Getpid()), Ports: ports}
	if err := writeLeases(leases); err != nil {
		removePorts(ip, ports)
		return fail(err)
	}
	return a, nil
}

// Detach takes a container off the bridge: its ports, its veth (if the
// end of its namespace has not taken it already) and its lease.
func (a *Attachment) Detach() error {
	unlock, err := lock()
	if err != nil {
		return err
	}
	defer unlock()
	removePorts(a.IP, a.Ports)
	if s, err := netlink.Open(); err == nil {
		if idx, err := s.LinkIndex(a.HostVeth); err == nil {
			_ = s.DeleteLink(idx)
		}
		s.Close()
	}
	leases, err := readLeases()
	if err != nil {
		return err
	}
	if l, ok := leases[a.IP.String()]; ok && l.ID == a.ID {
		delete(leases, a.IP.String())
	}
	return writeLeases(leases)
}

// freeAddress is the lowest address of the subnet not leased (.2 to .254;
// .1 is the bridge).
func freeAddress(leases map[string]lease) (net.IP, error) {
	base := subnetIP.To4()
	for last := 2; last < 255; last++ {
		ip := net.IPv4(base[0], base[1], base[2], byte(last)).To4()
		if _, taken := leases[ip.String()]; !taken {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("no free address in %s: 253 containers are on the bridge", subnetCIDR)
}

// ensureBridge makes the bridge, its address, forwarding and the nftables
// table, unless they are there already.
func ensureBridge() error {
	s, err := netlink.Open()
	if err != nil {
		return err
	}
	defer s.Close()
	idx, err := s.LinkIndex(BridgeName)
	if errors.Is(err, unix.ENODEV) {
		if err := s.CreateBridge(BridgeName); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		idx, err = s.LinkIndex(BridgeName)
	}
	if err != nil {
		return err
	}
	if err := s.AddAddress(idx, gateway, prefixLen); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	if err := s.SetUp(idx); err != nil {
		return err
	}
	// Route the containers' traffic out; let DNAT to a container work for
	// connections to the host's own 127.0.0.1.
	for path, value := range map[string]string{
		"/proc/sys/net/ipv4/ip_forward":                             "1",
		"/proc/sys/net/ipv4/conf/" + BridgeName + "/route_localnet": "1",
	} {
		if err := os.WriteFile(path, []byte(value), 0); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if _, err := exec.LookPath("nft"); err != nil {
		return errors.New("the bridge network needs nftables (the nft command), which is not installed: install it, or use --network none or host")
	}
	if exec.Command("nft", "list", "table", "ip", "minibox").Run() == nil {
		return nil
	}
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nftables: %v: %s", err, out)
	}
	return nil
}

func portElement(ip net.IP, p Port) string {
	return fmt.Sprintf("%s . %d : %s . %d", p.Proto, p.Host, ip, p.Container)
}

func addPorts(ip net.IP, ports []Port) error {
	if len(ports) == 0 {
		return nil
	}
	var elems []string
	for _, p := range ports {
		elems = append(elems, portElement(ip, p))
	}
	out, err := exec.Command("nft", "add", "element", "ip", "minibox", "ports", "{ "+strings.Join(elems, ", ")+" }").CombinedOutput()
	if err != nil {
		return fmt.Errorf("publishing ports: %v: %s", err, out)
	}
	return nil
}

func removePorts(_ net.IP, ports []Port) {
	for _, p := range ports {
		_ = exec.Command("nft", "delete", "element", "ip", "minibox", "ports", fmt.Sprintf("{ %s . %d }", p.Proto, p.Host)).Run()
	}
}

// InsideConfig is what the container's init gets for this attachment.
func (a *Attachment) InsideConfig(hostname string) Inside {
	return Inside{
		Mode:       Bridge,
		IP:         a.IP,
		Prefix:     prefixLen,
		Gateway:    gateway,
		ResolvConf: hostResolvConf(),
		Hosts:      hostsFile(a.IP, hostname),
	}
}

// NoneConfig is a container with only loopback.
func NoneConfig(hostname string) Inside {
	return Inside{Mode: None, Hosts: hostsFile(nil, hostname)}
}

// hostResolvConf is the host's resolver settings, from systemd-resolved's
// list of upstream servers when it is running.
func hostResolvConf() string {
	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		if data, err := os.ReadFile(path); err == nil {
			if conf := resolvConf(string(data)); strings.Contains(conf, "nameserver") {
				return conf
			}
		}
	}
	return ""
}
