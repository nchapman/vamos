//go:build !unix

package backupbackend

import (
	"os"
	"os/exec"
)

func owned(os.FileInfo) bool     { return false }
func configureProcess(*exec.Cmd) {}
