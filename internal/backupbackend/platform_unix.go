//go:build unix

package backupbackend

import (
	"os"
	"os/exec"
	"syscall"
)

func owned(st os.FileInfo) bool { s, ok := st.Sys().(*syscall.Stat_t); return ok && s.Uid == 0 }
func configureProcess(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		e := syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		if e == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return e
	}
}
