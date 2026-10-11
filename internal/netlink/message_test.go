package netlink

import (
	"bytes"
	"testing"
)

func TestAttrEncoding(t *testing.T) {
	// "eth0\0" is 5 bytes: header 4 + 5 = 9, padded to 12.
	got := strAttr(3, "eth0").encode()
	want := []byte{9, 0, 3, 0, 'e', 't', 'h', '0', 0, 0, 0, 0}
	if !bytes.Equal(got, want) {
		t.Fatalf("strAttr = %v, want %v", got, want)
	}
	n := nested(18, strAttr(1, "veth")).encode()
	// Outer: 4 + inner (4 + 5 -> 12) = 16 bytes, type 18 with NLA_F_NESTED.
	if len(n) != 16 || native.Uint16(n[0:2]) != 16 || native.Uint16(n[2:4]) != 18|nlaFNested {
		t.Fatalf("nested = %v", n)
	}
	attrs, err := parseAttrs(append(strAttr(3, "eth0").encode(), u32Attr(10, 7).encode()...))
	if err != nil || string(attrs[3]) != "eth0\x00" || native.Uint32(attrs[10]) != 7 {
		t.Fatalf("parseAttrs = %v, %v", attrs, err)
	}
	if _, err := parseAttrs([]byte{200, 0, 3, 0, 'x'}); err == nil {
		t.Fatal("an attribute longer than its buffer was accepted")
	}
}

func TestMessages(t *testing.T) {
	one := encodeMessage(header{typ: 16, flags: 5, seq: 9}, []byte{1, 2, 3})
	two := encodeMessage(header{typ: 2, seq: 10}, []byte{4, 5, 6, 7})
	buf := append(append(append([]byte{}, one...), 0), two...) // the first padded to 20
	hs, bodies, err := splitMessages(buf)
	if err != nil || len(hs) != 2 {
		t.Fatalf("splitMessages: %v, %v", hs, err)
	}
	if hs[0].typ != 16 || hs[0].seq != 9 || !bytes.Equal(bodies[0], []byte{1, 2, 3}) || hs[1].typ != 2 || !bytes.Equal(bodies[1], []byte{4, 5, 6, 7}) {
		t.Fatalf("decoded %+v %v", hs, bodies)
	}
	if _, _, err := splitMessages(one[:10]); err == nil {
		t.Fatal("a truncated header was accepted")
	}
	bad := append([]byte{}, one...)
	native.PutUint32(bad[0:4], 400)
	if _, _, err := splitMessages(bad); err == nil {
		t.Fatal("a message longer than its buffer was accepted")
	}
}

func TestIfinfomsgRoundTrip(t *testing.T) {
	m := ifinfomsg{family: 0, typ: 1, index: 42, flags: 1, change: 1}
	got, err := decodeIfinfomsg(m.encode())
	if err != nil || got != m {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
}
