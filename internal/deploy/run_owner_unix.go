//go:build !windows

package deploy

import (
	"os"
	"syscall"
)

func runPrivate(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && info.Mode().Perm() == 0600 && ok && st.Uid == uint32(os.Geteuid())
}
