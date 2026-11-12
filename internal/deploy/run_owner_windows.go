//go:build windows

package deploy

import "os"

// Unix ownership/mode enforcement applies on production hosts, not Windows tooling.
func runPrivate(info os.FileInfo) bool { return info.Mode().IsRegular() }
