//go:build linux

package netlink

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// vethInfoPeer is VETH_INFO_PEER from linux/veth.h: the peer's own
// ifinfomsg and attributes, inside a veth's IFLA_INFO_DATA.
const vethInfoPeer = 1

// Socket is a NETLINK_ROUTE socket in the network namespace of the thread
// that opened it.
type Socket struct {
	fd  int
	seq uint32
}

// Open opens a route netlink socket.
func Open() (*Socket, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("netlink bind: %w", err)
	}
	return &Socket{fd: fd}, nil
}

// Close closes the socket.
func (s *Socket) Close() error { return unix.Close(s.fd) }

// Error is the kernel's answer to a request it refused.
type Error struct {
	Op    string
	Errno syscall.Errno
}

func (e *Error) Error() string { return fmt.Sprintf("netlink %s: %v", e.Op, e.Errno) }
func (e *Error) Unwrap() error { return e.Errno }

// request sends one message and reads replies until the kernel's
// acknowledgement for it (an NLMSG_ERROR with error 0), returning the
// bodies of any other messages it sent first (a GET's answer).
func (s *Socket) request(op string, typ uint16, flags uint16, body []byte) ([][]byte, error) {
	s.seq++
	msg := encodeMessage(header{typ: typ, flags: unix.NLM_F_REQUEST | unix.NLM_F_ACK | flags, seq: s.seq}, body)
	if err := unix.Sendto(s.fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("netlink %s: send: %w", op, err)
	}
	var replies [][]byte
	buf := make([]byte, 1<<16)
	for {
		n, _, err := unix.Recvfrom(s.fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, fmt.Errorf("netlink %s: receive: %w", op, err)
		}
		hs, bodies, err := splitMessages(buf[:n])
		if err != nil {
			return nil, err
		}
		for i, h := range hs {
			if h.seq != s.seq {
				continue // an answer to something else
			}
			switch h.typ {
			case unix.NLMSG_ERROR:
				if len(bodies[i]) < 4 {
					return nil, fmt.Errorf("netlink %s: short error message", op)
				}
				// A negative errno; 0 is the acknowledgement.
				if code := int32(native.Uint32(bodies[i][0:4])); code != 0 {
					return nil, &Error{op, syscall.Errno(-code)}
				}
				return replies, nil
			case unix.NLMSG_DONE:
				return replies, nil
			default:
				replies = append(replies, append([]byte{}, bodies[i]...))
			}
		}
	}
}

// LinkIndex finds a link by name.
func (s *Socket) LinkIndex(name string) (int, error) {
	body := append(ifinfomsg{family: unix.AF_UNSPEC}.encode(), strAttr(unix.IFLA_IFNAME, name).encode()...)
	replies, err := s.request("get link "+name, unix.RTM_GETLINK, 0, body)
	if err != nil {
		return 0, err
	}
	for _, r := range replies {
		m, err := decodeIfinfomsg(r)
		if err != nil {
			return 0, err
		}
		return int(m.index), nil
	}
	return 0, fmt.Errorf("netlink: no link %s", name)
}

// newLink sends RTM_NEWLINK with NLM_F_CREATE|NLM_F_EXCL.
func (s *Socket) newLink(op string, msg ifinfomsg, attrs ...attr) error {
	body := append(msg.encode(), encodeAttrs(attrs)...)
	_, err := s.request(op, unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, body)
	return err
}

// CreateBridge makes a bridge device.
func (s *Socket) CreateBridge(name string) error {
	return s.newLink("create bridge "+name, ifinfomsg{family: unix.AF_UNSPEC},
		strAttr(unix.IFLA_IFNAME, name),
		nested(unix.IFLA_LINKINFO, strAttr(unix.IFLA_INFO_KIND, "bridge")),
	)
}

// CreateVeth makes a veth pair: name here, peer in the network namespace
// of process peerPID (a container's init), with the name peerName there.
func (s *Socket) CreateVeth(name, peerName string, peerPID int) error {
	peer := append(ifinfomsg{family: unix.AF_UNSPEC}.encode(), encodeAttrs([]attr{
		strAttr(unix.IFLA_IFNAME, peerName),
		u32Attr(unix.IFLA_NET_NS_PID, uint32(peerPID)),
	})...)
	return s.newLink("create veth "+name, ifinfomsg{family: unix.AF_UNSPEC},
		strAttr(unix.IFLA_IFNAME, name),
		nested(unix.IFLA_LINKINFO,
			strAttr(unix.IFLA_INFO_KIND, "veth"),
			nested(unix.IFLA_INFO_DATA, attr{typ: vethInfoPeer, data: peer}),
		),
	)
}

// setLink changes an existing link (RTM_NEWLINK on its index).
func (s *Socket) setLink(op string, msg ifinfomsg, attrs ...attr) error {
	body := append(msg.encode(), encodeAttrs(attrs)...)
	_, err := s.request(op, unix.RTM_NEWLINK, 0, body)
	return err
}

// SetUp sets a link up.
func (s *Socket) SetUp(index int) error {
	return s.setLink("set up", ifinfomsg{family: unix.AF_UNSPEC, index: int32(index), flags: unix.IFF_UP, change: unix.IFF_UP})
}

// SetMaster attaches a link to a bridge.
func (s *Socket) SetMaster(index, master int) error {
	return s.setLink("set master", ifinfomsg{family: unix.AF_UNSPEC, index: int32(index)}, u32Attr(unix.IFLA_MASTER, uint32(master)))
}

// DeleteLink removes a link (a veth's peer goes with it).
func (s *Socket) DeleteLink(index int) error {
	_, err := s.request("delete link", unix.RTM_DELLINK, 0, ifinfomsg{family: unix.AF_UNSPEC, index: int32(index)}.encode())
	return err
}

// AddAddress gives a link an IPv4 address with its prefix length.
func (s *Socket) AddAddress(index int, ip net.IP, prefix int) error {
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("netlink: %s is not IPv4", ip)
	}
	msg := make([]byte, ifaddrmsgLen)
	msg[0] = unix.AF_INET
	msg[1] = uint8(prefix)
	msg[3] = unix.RT_SCOPE_UNIVERSE
	native.PutUint32(msg[4:8], uint32(index))
	body := append(msg, encodeAttrs([]attr{
		{typ: unix.IFA_LOCAL, data: ip4},
		{typ: unix.IFA_ADDRESS, data: ip4},
	})...)
	_, err := s.request("add address "+ip.String(), unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL, body)
	return err
}

// AddDefaultRoute routes everything not on a local network via gateway,
// out of link index.
func (s *Socket) AddDefaultRoute(gateway net.IP, index int) error {
	gw := gateway.To4()
	if gw == nil {
		return fmt.Errorf("netlink: %s is not IPv4", gateway)
	}
	msg := make([]byte, rtmsgLen)
	msg[0] = unix.AF_INET
	msg[4] = unix.RT_TABLE_MAIN
	msg[5] = unix.RTPROT_BOOT
	msg[6] = unix.RT_SCOPE_UNIVERSE
	msg[7] = unix.RTN_UNICAST
	body := append(msg, encodeAttrs([]attr{
		{typ: unix.RTA_GATEWAY, data: gw},
		u32Attr(unix.RTA_OIF, uint32(index)),
	})...)
	_, err := s.request("add default route", unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_EXCL, body)
	return err
}
