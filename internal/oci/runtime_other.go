//go:build !linux

package oci

import (
	"fmt"
	"runtime"
)

func linuxOnly() error {
	return fmt.Errorf("containers need Linux, not %s (on a Mac, the Lima VM in lima/minibox.yaml)", runtime.GOOS)
}

// Create fails anywhere but Linux.
func Create(id, bundle string) error { return linuxOnly() }

// Start fails anywhere but Linux.
func Start(id string) error { return linuxOnly() }

// GetState fails anywhere but Linux.
func GetState(id string) (*State, error) { return nil, linuxOnly() }

// Kill fails anywhere but Linux.
func Kill(id, signal string) error { return linuxOnly() }

// Delete fails anywhere but Linux.
func Delete(id string, force bool) error { return linuxOnly() }
