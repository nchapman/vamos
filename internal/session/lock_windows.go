//go:build windows

package session

import (
	"errors"
	"os"
)

var errLocked = errors.New("session lock is held")

// Remote session staging is a Unix host facility. Fail closed on Windows while
// keeping the package available to cross-compiled CLI entrypoints.
func owned(os.FileInfo) bool { return false }

func lockFile(*os.Root, string, bool) (*os.File, error) {
	return nil, errors.New("session staging requires a Unix host")
}
