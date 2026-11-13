//go:build windows

package deploy

import "os"

func inventoryOwned(info os.FileInfo) bool { return true }
