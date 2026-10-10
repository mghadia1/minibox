package container

import (
	"slices"
	"strings"
	"testing"
)

const passwd = `root:x:0:0:root:/root:/bin/sh
# a comment
daemon:x:2:2:daemon:/sbin:/sbin/nologin
nobody:x:65534:65534:nobody:/:/sbin/nologin
app:x:1000:1000::/home/app:/bin/sh
broken:x:abc:def::/:/bin/sh
short:x:5
`

const group = `root:x:0:root
wheel:x:10:root,app
audio:x:18:app
app:x:1000:
nogroup:x:65533:
`

func TestResolveUser(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want Credentials
	}{
		{"root", Credentials{0, 0, []uint32{10}, "/root"}},
		{"app", Credentials{1000, 1000, []uint32{10, 18}, "/home/app"}},
		{"1000", Credentials{1000, 1000, []uint32{10, 18}, "/home/app"}},
		{"app:audio", Credentials{1000, 18, []uint32{10}, "/home/app"}},
		{"app:65533", Credentials{1000, 65533, []uint32{10, 18}, "/home/app"}},
		{"4242", Credentials{4242, 0, nil, ""}},
		{"4242:4243", Credentials{4242, 4243, nil, ""}},
		{"nobody", Credentials{65534, 65534, nil, "/"}},
	} {
		got, err := ResolveUser(tc.spec, strings.NewReader(passwd), strings.NewReader(group))
		if err != nil {
			t.Errorf("ResolveUser(%q): %v", tc.spec, err)
			continue
		}
		if got.UID != tc.want.UID || got.GID != tc.want.GID || !slices.Equal(got.Groups, tc.want.Groups) || got.Home != tc.want.Home {
			t.Errorf("ResolveUser(%q) = %+v, want %+v", tc.spec, got, tc.want)
		}
	}
	for _, bad := range []string{"", ":", "app:", ":audio", "ghost", "app:ghosts", "broken", "-1"} {
		if c, err := ResolveUser(bad, strings.NewReader(passwd), strings.NewReader(group)); err == nil {
			t.Errorf("ResolveUser(%q) = %+v, want an error", bad, c)
		}
	}
	// No passwd at all (a scratch image): numbers still work.
	if c, err := ResolveUser("10:20", nil, nil); err != nil || c.UID != 10 || c.GID != 20 {
		t.Errorf("without /etc/passwd: %+v, %v", c, err)
	}
}
