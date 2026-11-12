package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nchapman/soar/internal/cli"
)

// Overridden for release builds with -ldflags "-X main.version=<version>".
var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := shutdownContext()
	defer stop()
	return cli.RunContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version)
}

func shutdownContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
