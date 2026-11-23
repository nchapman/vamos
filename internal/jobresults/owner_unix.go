//go:build !windows

package jobresults

import (
	"os"
	"syscall"
)

func privateEntry(info os.FileInfo, perm os.FileMode) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid()) && info.Mode().Perm() == perm && info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}
