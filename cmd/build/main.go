// Command build produces a stamped native client and both Linux executors.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/nchapman/vamos/internal/buildbundle"
)

func main() {
	version := flag.String("version", "dev", "version label shared by the whole bundle")
	output := flag.String("out", "bin", "output directory")
	clientOS := flag.String("os", runtime.GOOS, "target client GOOS")
	clientArch := flag.String("arch", runtime.GOARCH, "target client GOARCH")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/build [-version LABEL] [-out DIRECTORY] [-os GOOS] [-arch GOARCH]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := buildbundle.BuildFor(ctx, ".", *output, *version, *clientOS, *clientArch, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
}
