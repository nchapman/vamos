package main

import (
	"os"

	"github.com/nchapman/soar/internal/cli"
)

// Overridden for release builds with -ldflags "-X main.version=<version>".
var version = "dev"

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, version))
}
