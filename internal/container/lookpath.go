package container

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// errNotFound and errNotExecutable become exit codes 127 and 126.
var (
	errNotFound      = errors.New("executable file not found in $PATH")
	errNotExecutable = errors.New("not an executable file")
)

// lookPath finds a command as a shell would, in the container's own PATH
// (not the host's: os/exec's LookPath reads this process's environment).
// A name with a slash is used as it is. It runs inside the container,
// after pivot_root, so every path is the container's.
func lookPath(name, pathEnv string) (string, error) {
	if strings.Contains(name, "/") {
		return name, checkExecutable(name)
	}
	var denied error
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "." // an empty entry means the current directory
		}
		candidate := filepath.Join(dir, name)
		switch err := checkExecutable(candidate); {
		case err == nil:
			return candidate, nil
		case errors.Is(err, errNotExecutable) && denied == nil:
			denied = err // found but not executable: keep looking, as sh does
		}
	}
	if denied != nil {
		return "", denied
	}
	return "", errNotFound
}

func checkExecutable(file string) error {
	info, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return errNotExecutable
	}
	return nil
}
