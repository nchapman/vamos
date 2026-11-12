package secrets

import "os"

// Unix ownership is enforced on production hosts; Windows supports local tooling.
func owned(info os.FileInfo) bool     { return true }
func productionOwner(dir string) bool { return true }
