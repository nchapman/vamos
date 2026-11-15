//go:build !windows

package agent

import (
	"errors"
	"os"
	"syscall"
)

func ownedByProcess(info os.FileInfo) bool {
	if info.Mode().Perm()&0022 != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid())
}

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
