package oci

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Container statuses, as the runtime spec names them.
const (
	StatusCreating = "creating"
	StatusCreated  = "created"
	StatusRunning  = "running"
	StatusStopped  = "stopped"
)

// ociVersion is the runtime spec version MiniBox reports in state.
const ociVersion = "1.0.2"

// Root is where per-container state lives. /run is a tmpfs: like the
// containers themselves, it does not outlive a reboot.
var StateRoot = "/run/minibox/oci"

// State is what `minibox state` prints, the runtime spec's State object.
type State struct {
	OCIVersion string `json:"ociVersion"`
	ID         string `json:"id"`
	Status     string `json:"status"`
	Pid        int    `json:"pid"`
	Bundle     string `json:"bundle"`
	Created    string `json:"created,omitempty"`
}

func dir(id string) string       { return filepath.Join(StateRoot, id) }
func fifoPath(id string) string  { return filepath.Join(dir(id), "exec.fifo") }
func logPath(id string) string   { return filepath.Join(dir(id), "output.log") }
func statePath(id string) string { return filepath.Join(dir(id), "state.json") }

// validID refuses an id that is empty or would escape the state root.
func validID(id string) error {
	if id == "" || strings.ContainsAny(id, "/\x00") || id == "." || id == ".." {
		return fmt.Errorf("invalid container id %q", id)
	}
	return nil
}

func writeState(s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath(s.ID) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(s.ID))
}

func readState(id string) (*State, error) {
	data, err := os.ReadFile(statePath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("container %q does not exist", id)
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", statePath(id), err)
	}
	return &s, nil
}

// exists reports whether a container id is already in use.
func exists(id string) bool {
	_, err := os.Stat(statePath(id))
	return err == nil
}

// List returns the ids of all known containers.
func List() ([]string, error) {
	entries, err := os.ReadDir(StateRoot)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && exists(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}
