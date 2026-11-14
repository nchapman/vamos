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
	"github.com/nchapman/soar/internal/registry"
)

const help = `Soar — a small app platform for one server

Usage:
  soar apps [--server NAME] [--json]      List app deployment and retained state
  soar init APP --port PORT              Write a minimal configuration without overwriting
  soar exec [--service NAME] [-i] [-t] -- COMMAND
                                        Run a command inside an existing service
  soar console [--service NAME]          Open an interactive shell in an existing service
  soar run --service NAME -- COMMAND    Run a noninteractive command
  soar deploy [-f soar.yaml]             Build and deploy the app's services
  soar status [--json]                   Inspect the app's actual running state
  soar logs [--tail 100] [--follow]      Read a snapshot or follow one container's logs
  soar rollback [--service NAME] [--allow-downtime] [--yes]
                                        Preview or restore captured service images
  soar resume OPERATION                 Resume captured operation inputs
  soar cleanup [--yes]                  Preview or remove obsolete app resources
  soar destroy [--yes]                  Preview or remove an app
  soar service bind|remove NAME [flags]  Bind locally or preview/remove a service
  soar secrets stage|list [flags]        Stage a secret or list retained version metadata
  soar validate [-f soar.yaml]           Validate local app configuration
  soar server inspect [flags] TARGET    Inspect a host over SSH without provisioning
  soar server add [flags] NAME TARGET   Bootstrap a dedicated Ubuntu host
  soar server status NAME [TARGET]     Read the host lifecycle journal
  soar server teardown [flags] NAME    Remove an unused owned installation
  soar backup repository [COMMAND]      Register and inspect backup repositories
  soar version                          Print the build version
  soar help [COMMAND [SUBCOMMAND [ACTION]]]      Show general or command-specific help

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
		_ = writeMainHelp(stdout)
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		if len(args) != 1 {
			if args[0] == "help" {
				if command, ok := commandHelpArgs(args[1:]); ok {
					return RunContext(ctx, command, stdin, stdout, stderr, version)
				}
			}
			return usageError(stderr, "help accepts only a public command and optional subcommand, not flags or arguments")
		}
		_ = writeMainHelp(stdout)
		return 0
	case "version", "--version":
		if args[0] == "version" && len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
			fmt.Fprintln(stdout, "Usage: soar version\nPrint the build version.")
			return 0
		}
		if len(args) != 1 {
			return usageError(stderr, "version does not accept arguments")
		}
		fmt.Fprintf(stdout, "soar %s\n", version)
		return 0
	case "backup":
		return backupCommand(ctx, args[1:], stdin, stdout, stderr, defaultAppDeps(), readSecretInput)
	case "apps":
		return appsCommand(ctx, args[1:], stdout, stderr, defaultAppDeps())
	case "init":
		return initCommand(args[1:], stdout, stderr, registry.Default)
	case "exec", "console":
		return sessionCommand(ctx, args[0], args[1:], stdin, stdout, stderr, defaultAppDeps(), terminalStreams)
	case "internal-session-prepare", "internal-session-attach", "internal-session-cleanup":
		return internalSession(ctx, args[0], args[1:], stdin, stdout, stderr)
	case "validate":
		return validate(args[1:], stdout, stderr)
	case "service":
		return serviceCommand(ctx, args[1:], stdout, stderr, defaultAppDeps())
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
	if err := parseFlags(flags, args, stdout, stderr); err != nil {
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
		return lifecycleError(stderr, err)
	}
	if view := presentationFor(stdout); view != nil {
		text := view.header("validate", app.Name, "", "") + view.ink("success", "Local configuration valid", true) + "\n"
		text += view.field("File", *filename) + view.field("Services", fmt.Sprint(len(app.Services)))
		text += view.prose("Local validation only; build files, images, secrets, DNS, and server state were not checked.")
		if err := view.write(stdout, text); err != nil {
			return lifecycleError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "%s: valid configuration for %s (%d services)\n", *filename, app.Name, len(app.Services))
	fmt.Fprintln(stdout, "Local validation only; build files, images, secrets, DNS, and server state were not checked.")
	return 0
}

func usageError(stderr io.Writer, message string) int {
	if view := presentationFor(stderr); view != nil {
		_ = view.write(stderr, view.ink("error", "Usage error", true)+"\n"+view.prose(message)+"\n"+view.command("soar help"))
		return 2
	}
	fmt.Fprintf(stderr, "error: %s\nRun 'soar help' for usage.\n", message)
	return 2
}
