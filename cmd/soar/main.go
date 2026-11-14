package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nchapman/soar/internal/agent"
	"github.com/nchapman/soar/internal/buildmeta"
	"github.com/nchapman/soar/internal/cli"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := shutdownContext()
	defer stop()
	if len(os.Args) >= 2 && os.Args[1] == "internal-agent" {
		if len(os.Args) == 2 {
			if err := agent.ServeSystem(ctx); err != nil {
				_, _ = os.Stderr.WriteString("soar-agent: " + err.Error() + "\n")
				return 1
			}
			return 0
		}
		if len(os.Args) == 5 && os.Args[2] == "bridge" && os.Args[3] == "--principal" {
			if err := agent.Bridge(ctx, agent.DefaultSocket, os.Args[4], os.Stdin, os.Stdout, os.Stderr); err != nil {
				_, _ = os.Stderr.WriteString("soar-agent bridge: " + err.Error() + "\n")
				return 1
			}
			return 0
		}
		_, _ = os.Stderr.WriteString("usage: internal-agent [bridge --principal PRINCIPAL]\n")
		return 2
	}
	return cli.RunContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, buildmeta.Version())
}

func shutdownContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
