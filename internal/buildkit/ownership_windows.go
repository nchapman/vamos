package buildkit

import "os"

// Windows is a supported client, never a privileged build-service host.
func ownedFile(os.FileInfo) bool { return false }
