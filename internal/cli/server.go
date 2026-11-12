package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/nchapman/soar/internal/host"
	"github.com/nchapman/soar/internal/ssh"
)

const serverHelp = `Usage:
  soar server inspect [--json] [--timeout 15s] TARGET
  soar server add --yes --domain DOMAIN [--executor FILE] NAME TARGET
  soar server status [--json] NAME [TARGET]
  soar server teardown --yes --purge [--executor FILE] NAME [TARGET]

TARGET is an SSH alias or [user@]host (unbracketed IPv6 is supported).
Use SSH config for ports, identities, and jump hosts. Flags precede TARGET.
Requires an already trusted host key and noninteractive key/agent authentication.
Inspect is read-only. Add and teardown require root and explicit --yes.
Teardown refuses workloads and changed managed resources; --purge deletes owned data.
TARGET is optional for registered names; provide it to recover an unregistered host.
`

func server(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help")) {
		fmt.Fprint(stdout, serverHelp)
		return 0
	}
	switch args[0] {
	case "inspect":
		return inspectServer(ctx, args[1:], stdout, stderr, ssh.Inspect)
	case "add", "status", "teardown":
		return serverLifecycle(ctx, args[0], args[1:], stdout, stderr, defaultLifecycleDeps())
	default:
		return usageError(stderr, "unknown server command")
	}
}

func inspectServer(ctx context.Context, args []string, stdout, stderr io.Writer, inspect func(context.Context, string) (*host.Report, error)) int {
	flags := flag.NewFlagSet("server inspect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "print the inspection report as JSON")
	timeout := flags.Duration("timeout", 15*time.Second, "overall inspection deadline")
	flags.Usage = func() { fmt.Fprint(flags.Output(), serverHelp) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 1 || *timeout <= 0 {
		return usageError(stderr, "inspect requires one TARGET and a positive timeout; flags must precede TARGET")
	}
	target := flags.Arg(0)
	if err := ssh.ValidateTarget(target); err != nil {
		return usageError(stderr, err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	report, err := inspect(ctx, target)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			fmt.Fprintf(stderr, "error: write report: %v\n", err)
			return 1
		}
	} else {
		var text bytes.Buffer
		fmt.Fprintf(&text, "Host: %s\nOS: %s %s\nKernel: %s\nArchitecture: %s\nSystemd: %t\nUID: %d\nCompatible host: %t\n", target, report.OS, report.Version, report.Kernel, report.Architecture, report.Systemd, report.UID, report.Supported)
		if report.Root {
			fmt.Fprintln(&text, "Privilege: root (no setup changes made)")
		} else {
			fmt.Fprintln(&text, "Privilege: non-root; privileged setup capability was not checked (sudo was not invoked)")
		}
		for _, problem := range report.Problems {
			fmt.Fprintf(&text, "- %s\n", problem)
		}
		if _, err := io.Copy(stdout, &text); err != nil {
			fmt.Fprintf(stderr, "error: write report: %v\n", err)
			return 1
		}
	}
	if !report.Supported {
		return 1
	}
	return 0
}
