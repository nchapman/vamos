// Command build produces a stamped native client and both Linux executors.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nchapman/soar/internal/buildbundle"
)

func main() {
	version := flag.String("version", "dev", "version label shared by the whole bundle")
	output := flag.String("out", "bin", "output directory")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/build [-version LABEL] [-out DIRECTORY]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := buildbundle.Build(ctx, ".", *output, *version, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
}
