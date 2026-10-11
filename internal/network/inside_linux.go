//go:build linux

package network

import (
	"fmt"
	"os"

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
		if err := os.MkdirAll("/etc", 0o755); err != nil {
			return err
		}
		// Replace, not write through: in an image, /etc/resolv.conf is
		// sometimes a link to somewhere that is not there.
		_ = os.Remove(path)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}
