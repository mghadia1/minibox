package container

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Credentials is who the command runs as.
type Credentials struct {
	UID, GID uint32
	Groups   []uint32 // supplementary groups
	Home     string   // from the user's passwd entry, "" if unknown
}

// ResolveUser reads a user as Docker's --user and an image's User take
// it: "name", "uid", "name:group", "uid:gid" (any mix), looked up in the
// container's own /etc/passwd and /etc/group. A number not in passwd is
// used as it is, with gid 0 unless one is given. Supplementary groups are
// the groups that list the user by name.
func ResolveUser(spec string, passwd, group io.Reader) (Credentials, error) {
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	if userPart == "" || (hasGroup && groupPart == "") {
		return Credentials{}, fmt.Errorf("user %q is not NAME[:GROUP] or UID[:GID]", spec)
	}
	users, err := readEntries(passwd, 7)
	if err != nil {
		return Credentials{}, fmt.Errorf("/etc/passwd: %w", err)
	}
	groups, err := readEntries(group, 4)
	if err != nil {
		return Credentials{}, fmt.Errorf("/etc/group: %w", err)
	}

	var c Credentials
	name := ""
	if uid, ok := number(userPart); ok {
		c.UID = uid
		for _, u := range users { // a numeric user still gets its entry's group and home
			if u[2] == userPart {
				name, c.Home = u[0], u[5]
				c.GID, _ = number(u[3])
				break
			}
		}
	} else {
		found := false
		for _, u := range users {
			if u[0] == userPart {
				uid, ok1 := number(u[2])
				gid, ok2 := number(u[3])
				if !ok1 || !ok2 {
					return Credentials{}, fmt.Errorf("/etc/passwd entry for %q has no numeric uid and gid", userPart)
				}
				name, c.UID, c.GID, c.Home, found = u[0], uid, gid, u[5], true
				break
			}
		}
		if !found {
			return Credentials{}, fmt.Errorf("no user %q in the container's /etc/passwd", userPart)
		}
	}

	if hasGroup {
		if gid, ok := number(groupPart); ok {
			c.GID = gid
		} else {
			found := false
			for _, g := range groups {
				if g[0] == groupPart {
					gid, ok := number(g[2])
					if !ok {
						return Credentials{}, fmt.Errorf("/etc/group entry for %q has no numeric gid", groupPart)
					}
					c.GID, found = gid, true
					break
				}
			}
			if !found {
				return Credentials{}, fmt.Errorf("no group %q in the container's /etc/group", groupPart)
			}
		}
	}
	if name != "" {
		for _, g := range groups {
			for _, member := range strings.Split(g[3], ",") {
				if member == name {
					if gid, ok := number(g[2]); ok && gid != c.GID {
						c.Groups = append(c.Groups, gid)
					}
				}
			}
		}
	}
	return c, nil
}

// readEntries reads colon-separated lines with at least n fields
// (/etc/passwd has 7, /etc/group 4); comments and short lines are
// skipped. A missing file (nil) has no entries.
func readEntries(r io.Reader, n int) ([][]string, error) {
	if r == nil {
		return nil, nil
	}
	var out [][]string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) >= n {
			out = append(out, fields)
		}
	}
	return out, sc.Err()
}

func number(s string) (uint32, bool) {
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err == nil
}
