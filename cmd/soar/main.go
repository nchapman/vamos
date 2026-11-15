package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nchapman/soar/internal/agent"
	"github.com/nchapman/soar/internal/buildmeta"
	"github.com/nchapman/soar/internal/cli"
	"github.com/nchapman/soar/internal/executor"
)

func main() {
	os.Exit(run())
}

const forcedBridgePrefix = "/usr/local/lib/soar/soar-agent internal-agent bridge --principal "

func run() int {
	ctx, stop := shutdownContext()
	defer stop()
	// The restricted soar account uses this executable as its login shell.
	// Accept only its authorized_keys forced bridge command; any unmatched SSH
	// command (including a key added before sshd reloads) fails closed.
	if len(os.Args) == 3 && os.Args[1] == "-c" {
		principal, ok := strings.CutPrefix(os.Args[2], forcedBridgePrefix)
		if ok && !strings.ContainsAny(principal, " \t\r\n") {
			if err := agent.Bridge(ctx, agent.DefaultSocket, principal, os.Stdin, os.Stdout, os.Stderr); err != nil {
				return 1
			}
			return 0
		}
		return 126
	}
	if len(os.Args) >= 2 && os.Args[1] == "internal-agent" {
		if len(os.Args) == 2 {
			if err := agent.ServeSystem(ctx); err != nil {
				_, _ = os.Stderr.WriteString("soar-agent: " + err.Error() + "\n")
				return 1
			}
			return 0
		}
		if len(os.Args) == 4 && os.Args[2] == "worker" {
			return executor.ServeWorker(ctx, os.Args[3])
		}
		if len(os.Args) == 5 && os.Args[2] == "bridge" && os.Args[3] == "--principal" {
			if err := agent.Bridge(ctx, agent.DefaultSocket, os.Args[4], os.Stdin, os.Stdout, os.Stderr); err != nil {
				_, _ = os.Stderr.WriteString("soar-agent bridge: " + err.Error() + "\n")
				return 1
			}
			return 0
		}
		_, _ = os.Stderr.WriteString("usage: internal-agent [bridge --principal PRINCIPAL | worker WORKDIR]\n")
		return 2
	}
	return cli.RunContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, buildmeta.Version())
}

func shutdownContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
