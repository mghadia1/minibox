package network

import "testing"

func TestParsePort(t *testing.T) {
	for in, want := range map[string]Port{
		"8080:80":     {"tcp", 8080, 80},
		"53:53/udp":   {"udp", 53, 53},
		"80":          {"tcp", 80, 80},
		"65535:1/tcp": {"tcp", 65535, 1},
	} {
		if got, err := ParsePort(in); err != nil || got != want {
			t.Errorf("ParsePort(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "80:", ":80", "0:80", "80:65536", "80:80/sctp", "1.2.3.4:80:80", "-1"} {
		if p, err := ParsePort(bad); err == nil {
			t.Errorf("ParsePort(%q) = %+v, want an error", bad, p)
		}
	}
}

func TestResolvConf(t *testing.T) {
	host := "# comment\nnameserver 127.0.0.53\nnameserver 192.168.5.3\nnameserver ::1\nsearch lan\noptions edns0 trust-ad\nweird line\n"
	want := "nameserver 192.168.5.3\nsearch lan\noptions edns0 trust-ad\n"
	if got := resolvConf(host); got != want {
		t.Fatalf("resolvConf = %q, want %q", got, want)
	}
}

func TestParseMode(t *testing.T) {
	for _, m := range []string{"bridge", "none", "host"} {
		if _, err := ParseMode(m); err != nil {
			t.Errorf("%s refused", m)
		}
	}
	if _, err := ParseMode("container:x"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

func TestBoundPorts(t *testing.T) {
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 3500007F:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000   991        0 207893 1
   1: 0100007F:D174 0100007F:14E9 01 00000000:00000000 00:00000000 00000000     0        0 225491 2
`
	tcp6 := `  sl  local_address                         remote_address                        st tx_queue
   0: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A 00000000:00000000
`
	if got := boundPorts(tcp, "0A"); len(got) != 1 || !got[53] {
		t.Errorf("tcp listeners: %v, want only 53 (an established connection's port is not a listener)", got)
	}
	if got := boundPorts(tcp6, "0A"); len(got) != 1 || !got[22] {
		t.Errorf("tcp6 listeners: %v, want 22", got)
	}
	if got := boundPorts(tcp, "07"); len(got) != 0 {
		t.Errorf("state 07 in a table without it: %v", got)
	}
}
