//go:build !windows

package deploy

import (
	"os"
	"syscall"
)

func inventoryOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
