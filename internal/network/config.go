// Package network connects containers: a bridge on the host, a veth pair
// per container (one end on the bridge, the other as eth0 inside), an
// address for each from a small lease file, NAT out through the host,
// and published ports, as Docker's default bridge network does.
package network

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Mode is how a container is connected.
type Mode string

const (
	Bridge Mode = "bridge" // its own network namespace, on MiniBox's bridge (the default)
	None   Mode = "none"   // its own network namespace, with only loopback
	Host   Mode = "host"   // the host's network namespace: no isolation at all
)

// ParseMode reads --network.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case Bridge, None, Host:
		return m, nil
	}
	return "", fmt.Errorf("--network %q: bridge, none or host", s)
}

// The bridge and its subnet. 10.99.0.0/24: private, and not Docker's
// (172.17.0.0/16) or Podman's (10.88.0.0/16), so they can live together.
const (
	BridgeName = "minibox0"
	subnetCIDR = "10.99.0.0/24"
)

var (
	subnetIP, subnet, _ = net.ParseCIDR(subnetCIDR)
	gateway             = net.IPv4(10, 99, 0, 1).To4()
	prefixLen, _        = subnet.Mask.Size()
)

// Gateway is the bridge's own address: the containers' default route.
func Gateway() net.IP { return gateway }

// Port is one published port: host port to container port.
type Port struct {
	Proto     string `json:"proto"` // tcp or udp
	Host      int    `json:"host"`
	Container int    `json:"container"`
}

// ParsePort reads -p as Docker writes it: "8080:80", "8080:80/udp", "80"
// (the same number on both sides).
func ParsePort(s string) (Port, error) {
	spec, proto, hasProto := strings.Cut(s, "/")
	if !hasProto {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return Port{}, fmt.Errorf("port %q: protocol %q is not tcp or udp", s, proto)
	}
	hostPart, containerPart, two := strings.Cut(spec, ":")
	if !two {
		containerPart = hostPart
	}
	host, err1 := strconv.Atoi(hostPart)
	ctr, err2 := strconv.Atoi(containerPart)
	if err1 != nil || err2 != nil || host < 1 || host > 65535 || ctr < 1 || ctr > 65535 {
		return Port{}, fmt.Errorf("port %q is not HOST:CONTAINER[/tcp|/udp] with ports 1-65535", s)
	}
	return Port{Proto: proto, Host: host, Container: ctr}, nil
}

// Inside is what a container's init needs to set up its side.
type Inside struct {
	Mode    Mode   `json:"mode"`
	IP      net.IP `json:"ip,omitempty"`
	Prefix  int    `json:"prefix,omitempty"`
	Gateway net.IP `json:"gateway,omitempty"`
	// Files written into the container's /etc.
	ResolvConf string `json:"resolv_conf,omitempty"`
	Hosts      string `json:"hosts,omitempty"`
}

// resolvConf makes a container's /etc/resolv.conf from the host's,
// without the nameservers on loopback (systemd-resolved's 127.0.0.53 is
// the host's own, unreachable from a container): pass the upstream file
// (/run/systemd/resolve/resolv.conf) when there is one.
func resolvConf(host string) string {
	var out []string
	for line := range strings.Lines(host) {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			if ip := net.ParseIP(f[1]); ip == nil || ip.IsLoopback() {
				continue
			}
		}
		if len(f) > 0 && (f[0] == "nameserver" || f[0] == "search" || f[0] == "options") {
			out = append(out, strings.Join(f, " "))
		}
	}
	return strings.Join(out, "\n") + "\n"
}

// hostsFile is a container's /etc/hosts.
func hostsFile(ip net.IP, hostname string) string {
	s := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"
	if ip != nil {
		s += ip.String() + "\t" + hostname + "\n"
	} else {
		s += "127.0.1.1\t" + hostname + "\n"
	}
	return s
}

// boundPorts reads a socket table, /proc/net/tcp, tcp6, udp or udp6: the
// local ports of the sockets in state ("0A", listening, for TCP; "07",
// bound and not connected, for UDP). Each line is "sl local rem st ...",
// with local as ADDRESS:PORT in hex.
func boundPorts(table, state string) map[int]bool {
	ports := map[int]bool{}
	for line := range strings.Lines(table) {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != state {
			continue
		}
		i := strings.LastIndexByte(f[1], ':')
		if i < 0 {
			continue
		}
		if n, err := strconv.ParseUint(f[1][i+1:], 16, 16); err == nil {
			ports[int(n)] = true
		}
	}
	return ports
}
