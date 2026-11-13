// Package cli implements the local command-line interface and remote operations.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/executor"
)

const help = `Soar — a small app platform for one server

Usage:
  soar run --service NAME -- COMMAND    Run a noninteractive command
  soar deploy [-f soar.yaml]             Build and deploy the app's services
  soar status [--json]                   Inspect the app's actual running state
  soar logs [--tail 100]                 Read bounded service-container logs
  soar rollback [--service NAME] [--allow-downtime] [--yes]
                                        Preview or restore captured service images
  soar resume OPERATION                 Resume captured operation inputs
  soar cleanup [--yes]                  Preview or remove obsolete app resources
  soar destroy [--yes]                  Preview or remove an app
  soar secrets stage|list [flags]        Stage a secret or list retained version metadata
  soar validate [-f soar.yaml]           Validate local app configuration
  soar server inspect [flags] TARGET    Inspect a host over SSH without provisioning
  soar server add [flags] NAME TARGET   Bootstrap a dedicated Ubuntu host
  soar server status NAME [TARGET]     Read the host lifecycle journal
  soar server teardown [flags] NAME    Remove an unused owned installation
  soar version                          Print the build version
  soar help                             Show this help

Setup/teardown require explicit authorization. Deploy supports 1–8 built or prebuilt public/private
services, with one optional named volume per service. Use deploy --service NAME for scoped updates;
existing manual services require --yes. Data-backed updates stop/start; data is retained.
Run a command with --help for options.
`

// Run is the noninteractive entry point for local commands.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	return RunContext(context.Background(), args, strings.NewReader(""), stdout, stderr, version)
}

// RunContext returns 0 for success, 1 for validation/inspection failures or an
// unsupported host, and 2 for usage errors. A known one-off command returns its
// captured exit code. The internal protocol has its own
// documented exit semantics. Context cancellation is propagated to subprocesses.
func RunContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, version string) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, help)
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		if len(args) != 1 {
			return usageError(stderr, "help does not accept arguments")
		}
		fmt.Fprint(stdout, help)
		return 0
	case "version", "--version":
		if len(args) != 1 {
			return usageError(stderr, "version does not accept arguments")
		}
		fmt.Fprintf(stdout, "soar %s\n", version)
		return 0
	case "validate":
		return validate(args[1:], stdout, stderr)
	case "secrets":
		return secretsCommand(ctx, args[1:], stdin, stdout, stderr, defaultAppDeps(), readSecretInput)
	case "server":
		return server(ctx, args[1:], stdout, stderr)
	case "run", "deploy", "status", "logs", "rollback", "resume", "cleanup", "destroy":
		return appCommand(ctx, args[0], args[1:], stdout, stderr, defaultAppDeps())
	case "internal-app-executor":
		if len(args) != 1 {
			return usageError(stderr, "internal-app-executor accepts only a JSON request on stdin")
		}
		return executor.ServeApp(ctx, stdin, stdout)
	case "internal-executor":
		if len(args) != 1 {
			return usageError(stderr, "internal-executor accepts only a JSON request on stdin")
		}
		return executor.Serve(ctx, stdin, stdout)
	default:
		return usageError(stderr, fmt.Sprintf("unknown command %q", args[0]))
	}
}

func validate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	filename := flags.String("f", config.DefaultFile, "path to app configuration")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: soar validate [-f soar.yaml]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "validate does not accept positional arguments; use -f <path>")
	}
	app, err := config.Load(*filename)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: valid configuration for %s (%d services)\n", *filename, app.Name, len(app.Services))
	fmt.Fprintln(stdout, "Local validation only; build files, images, secrets, DNS, and server state were not checked.")
	return 0
}

func usageError(stderr io.Writer, message string) int {
	fmt.Fprintf(stderr, "error: %s\nRun 'soar help' for usage.\n", message)
	return 2
}
