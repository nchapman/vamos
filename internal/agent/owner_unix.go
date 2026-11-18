//go:build !windows

package agent

import (
	"errors"
	"os"
	"syscall"
)

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil && !errors.Is(syncErr, syscall.EINVAL) && !errors.Is(syncErr, syscall.ENOTSUP) && !errors.Is(syncErr, syscall.EBADF) {
		return syncErr
	}
	return closeErr
}
