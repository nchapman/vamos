package jobresults

import "os"

// Only the Linux host persists production history. Windows runs client and filesystem tests.
func privateEntry(os.FileInfo, os.FileMode) bool { return true }
