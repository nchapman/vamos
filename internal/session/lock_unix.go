//go:build !windows

package session

import (
	"errors"
	"os"
	"syscall"
)

var errLocked = errors.New("session lock is held")

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && (info.IsDir() || stat.Nlink == 1)
}

// Locks are nonblocking and use the validated, root-relative descriptor rather
// than reopening a path through a flock library. Close releases the lock.
func lockFile(dir *os.Root, name string, create bool) (*os.File, error) {
	if create {
		f, err := dir.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			err = f.Close()
		}
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	f, err := openPrivate(dir, name, os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errLocked
		}
		return nil, err
	}
	return f, nil
}
