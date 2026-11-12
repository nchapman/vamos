package volumes

import "os"

func owned(info os.FileInfo) bool     { return true }
func productionOwner(dir string) bool { return true }
