//go:build !windows

package secrets

import (
	"os"
	"path/filepath"
	"syscall"
)

func owned(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid())
}
func productionOwner(dir string) bool {
	return filepath.Clean(dir) != "/var/lib/vamos/secrets" || os.Geteuid() == 0
}
