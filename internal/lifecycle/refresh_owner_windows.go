package lifecycle

import "os"

// Windows is a supported client, never a privileged lifecycle host.
func refreshFileOwned(os.FileInfo) bool { return false }
