//go:build windows

package backup

import "os"

// The production repository store runs only in the root Linux executor.
func owned(info os.FileInfo) bool { return true }
