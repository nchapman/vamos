//go:build windows

package bridgeacl

import "os"

func owned(info os.FileInfo) bool { return info != nil }
