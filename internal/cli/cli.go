// Package cli implements the local command-line interface. No command currently
// contacts a server or changes Docker state.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/nchapman/soar/internal/config"
)

const help = `Soar — a small app platform for one server

Usage:
  soar validate [-f soar.yaml]   Validate local app configuration
  soar version                   Print the build version
  soar help                      Show this help

Deployment and server management are not implemented yet. See PLAN.md.
`

// Run returns a process exit code: 0 for success, 1 for configuration failures,
// and 2 for command-line usage errors. Streams are supplied by the caller.
func Run(args []string, stdout, stderr io.Writer, version string) int {
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
