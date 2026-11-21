//go:build !windows

package lifecycle

import (
	"os"
	"syscall"
)

func refreshFileOwned(st os.FileInfo) bool {
	stat, ok := st.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
