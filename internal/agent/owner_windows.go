//go:build windows

package agent

import "os"

func ownedByProcess(info os.FileInfo) bool {
	return info != nil
}

func syncDir(string) error {
	return nil
}
