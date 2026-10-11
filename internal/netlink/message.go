// Package netlink speaks just enough rtnetlink, written by hand, to wire
// up a container's network: make a bridge and a veth pair, move one end
// into the container's network namespace, attach the other to the
// bridge, set links up, add addresses and a default route.
//
// A netlink message is a 16-byte header (length, type, flags, sequence
// number, port id), then a fixed structure for its type (ifinfomsg,
// ifaddrmsg, rtmsg), then attributes: each a 4-byte header (length, type)
// and its data, padded to a multiple of 4. Attributes nest: a veth's peer
// is a whole ifinfomsg with attributes of its own, inside IFLA_INFO_DATA
// inside IFLA_LINKINFO.
package netlink

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Native byte order: netlink is spoken between this process and its own
// kernel. (Every machine MiniBox runs on is little-endian.)
var native = binary.LittleEndian

const (
	headerLen    = 16
	attrHeadLen  = 4
	nlaFNested   = 0x8000
	ifinfomsgLen = 16
	ifaddrmsgLen = 8
	rtmsgLen     = 12
)

func align4(n int) int { return (n + 3) &^ 3 }

// attr is one attribute: its type and either raw data or nested ones.
type attr struct {
	typ      uint16
	data     []byte
	children []attr
}

func (a attr) encode() []byte {
	body := a.data
	typ := a.typ
	if a.children != nil {
		body = encodeAttrs(a.children)
		typ |= nlaFNested
	}
	out := make([]byte, attrHeadLen, align4(attrHeadLen+len(body)))
	native.PutUint16(out[0:2], uint16(attrHeadLen+len(body)))
	native.PutUint16(out[2:4], typ)
	out = append(out, body...)
	for len(out)%4 != 0 {
		out = append(out, 0)
	}
	return out
}

func encodeAttrs(attrs []attr) []byte {
	var out []byte
	for _, a := range attrs {
		out = append(out, a.encode()...)
	}
	return out
}

func strAttr(typ uint16, s string) attr { return attr{typ: typ, data: append([]byte(s), 0)} }

func u32Attr(typ uint16, v uint32) attr {
	b := make([]byte, 4)
	native.PutUint32(b, v)
	return attr{typ: typ, data: b}
}

func nested(typ uint16, children ...attr) attr {
	if children == nil {
		children = []attr{}
	}
	return attr{typ: typ, children: children}
}

// ifinfomsg is struct ifinfomsg: a link.
type ifinfomsg struct {
	family uint8
	typ    uint16
	index  int32
	flags  uint32
	change uint32
}

func (m ifinfomsg) encode() []byte {
	b := make([]byte, ifinfomsgLen)
	b[0] = m.family
	native.PutUint16(b[2:4], m.typ)
	native.PutUint32(b[4:8], uint32(m.index))
	native.PutUint32(b[8:12], m.flags)
	native.PutUint32(b[12:16], m.change)
	return b
}

func decodeIfinfomsg(b []byte) (ifinfomsg, error) {
	if len(b) < ifinfomsgLen {
		return ifinfomsg{}, errors.New("short ifinfomsg")
	}
	return ifinfomsg{
		family: b[0],
		typ:    native.Uint16(b[2:4]),
		index:  int32(native.Uint32(b[4:8])),
		flags:  native.Uint32(b[8:12]),
		change: native.Uint32(b[12:16]),
	}, nil
}

// header is struct nlmsghdr.
type header struct {
	length uint32
	typ    uint16
	flags  uint16
	seq    uint32
	pid    uint32
}

func encodeMessage(h header, body []byte) []byte {
	b := make([]byte, headerLen, headerLen+len(body))
	native.PutUint32(b[0:4], uint32(headerLen+len(body)))
	native.PutUint16(b[4:6], h.typ)
	native.PutUint16(b[6:8], h.flags)
	native.PutUint32(b[8:12], h.seq)
	native.PutUint32(b[12:16], h.pid)
	return append(b, body...)
}

// splitMessages cuts a buffer the kernel sent into messages.
func splitMessages(b []byte) ([]header, [][]byte, error) {
	var hs []header
	var bodies [][]byte
	for len(b) > 0 {
		if len(b) < headerLen {
			return nil, nil, fmt.Errorf("netlink: %d stray bytes", len(b))
		}
		h := header{
			length: native.Uint32(b[0:4]),
			typ:    native.Uint16(b[4:6]),
			flags:  native.Uint16(b[6:8]),
			seq:    native.Uint32(b[8:12]),
			pid:    native.Uint32(b[12:16]),
		}
		if h.length < headerLen || int(h.length) > len(b) {
			return nil, nil, fmt.Errorf("netlink: message length %d of %d bytes", h.length, len(b))
		}
		hs = append(hs, h)
		bodies = append(bodies, b[headerLen:h.length])
		next := align4(int(h.length))
		if next >= len(b) {
			break
		}
		b = b[next:]
	}
	return hs, bodies, nil
}

// parseAttrs reads a run of attributes into type -> data (the last of a
// type wins; nesting is left to the caller).
func parseAttrs(b []byte) (map[uint16][]byte, error) {
	out := map[uint16][]byte{}
	for len(b) >= attrHeadLen {
		l := int(native.Uint16(b[0:2]))
		typ := native.Uint16(b[2:4]) &^ nlaFNested
		if l < attrHeadLen || l > len(b) {
			return nil, fmt.Errorf("netlink: attribute length %d of %d bytes", l, len(b))
		}
		out[typ] = b[attrHeadLen:l]
		if align4(l) >= len(b) {
			break
		}
		b = b[align4(l):]
	}
	return out, nil
}
