//go:build !windows

package bridgeacl

import (
	"os"
	"syscall"
)

func owned(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0022 == 0
}
