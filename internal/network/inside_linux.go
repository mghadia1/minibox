//go:build linux

package network

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/mghadia1/minibox/internal/netlink"
)

// SetUpInside runs in the container's init, in its own network namespace
// and after pivot_root: loopback up, and for a bridged container eth0's
// address, eth0 up and the default route; then /etc/hosts and
// /etc/resolv.conf.
func SetUpInside(in Inside) error {
	s, err := netlink.Open()
	if err != nil {
		return err
	}
	defer s.Close()
	lo, err := s.LinkIndex("lo")
	if err != nil {
		return err
	}
	if err := s.SetUp(lo); err != nil {
		return err
	}
	if in.Mode == Bridge {
		eth, err := s.LinkIndex("eth0")
		if err != nil {
			return fmt.Errorf("eth0: %w", err)
		}
		if err := s.AddAddress(eth, in.IP, in.Prefix); err != nil {
			return err
		}
		if err := s.SetUp(eth); err != nil {
			return err
		}
		if err := s.AddDefaultRoute(in.Gateway, eth); err != nil {
			return err
		}
	}
	files := map[string]string{"/etc/hosts": in.Hosts}
	if in.ResolvConf != "" {
		files["/etc/resolv.conf"] = in.ResolvConf
	}
	for path, content := range files {
		if content == "" {
			continue
		}
		_ = os.MkdirAll("/etc", 0o755)
		// Replace, not write through: in an image, /etc/resolv.conf is
		// sometimes a link to somewhere that is not there.
		_ = os.Remove(path)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			// In a user namespace the root filesystem's files are owned by
			// the host's root, which is unmapped inside, so even the
			// container's root cannot replace them (MiniBox does not
			// id-map the root filesystem). The network itself is already
			// set up; leave /etc as the image shipped it. This never
			// happens without a user namespace, where Init is real root.
			if errors.Is(err, os.ErrPermission) || errors.Is(err, unix.EROFS) {
				continue
			}
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}
