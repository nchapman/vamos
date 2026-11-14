//go:build !windows

package buildkit

import (
	"os"
	"syscall"
)

func ownedFile(st os.FileInfo) bool {
	stat, ok := st.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
