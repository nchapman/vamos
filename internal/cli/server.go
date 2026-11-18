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

	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/ssh"
)

const serverUpgradeImpact = "Host upgrade restarts the Vamos agent and may replace Caddy or update the managed firewall. Caddy replacement can interrupt public traffic."

const serverHelp = `Usage:
  vamos server inspect [--json] [--timeout 15s] TARGET
  vamos server add --yes --firewall=true|false [--domain DOMAIN] [--executor FILE] NAME TARGET
  vamos server upgrade --yes [--executor FILE] [NAME [TARGET]]
  vamos server connect [--bridge] [--domain DOMAIN] NAME TARGET
  vamos server status [--json] [NAME [TARGET]]
  vamos server principal grant|list|revoke [flags]
  vamos server firewall enable|disable --yes [NAME]
  vamos server firewall status [--json] [NAME]
  vamos server teardown --yes --purge [--executor FILE] NAME [TARGET]

TARGET is an SSH alias or [user@]host (unbracketed IPv6 is supported).
Use SSH config for ports, identities, and jump hosts. Flags precede TARGET.
Requires an already trusted host key and noninteractive key/agent authentication.
Inspect is read-only. Connect verifies an existing ready host and registers it locally;
use --bridge to connect with only a deploy key instead of administrator SSH.
Add and teardown require root or passwordless sudo and explicit --yes.
Add prompts for a firewall choice on interactive stdin; scripts must pass --firewall=true|false.
Managed firewall is opt-in dedicated-host protection with automatic Docker public publishing.
Disable removes protection only; unsupported existing firewall configurations are refused.
` + serverUpgradeImpact + `
Teardown refuses workloads and changed managed resources; --purge deletes owned data.
TARGET is optional for registered names; provide it to recover an unregistered host.
`

func serverWithInput(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	deps := defaultLifecycleDeps()
	deps.Input = stdin
	if len(args) == 0 || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help")) {
		_ = writeHelpText(stdout, serverHelp)
		return 0
	}
	switch args[0] {
	case "inspect":
		return inspectServer(ctx, args[1:], stdout, stderr, ssh.Inspect)
	case "principal":
		return serverPrincipal(ctx, args[1:], stdout, stderr, deps)
	case "firewall":
		return serverFirewall(ctx, args[1:], stdout, stderr, deps)
	case "add", "upgrade", "connect", "status", "teardown":
		return serverLifecycle(ctx, args[0], args[1:], stdout, stderr, deps)
	default:
		return usageError(stderr, "unknown server command")
	}
}

func inspectServer(ctx context.Context, args []string, stdout, stderr io.Writer, inspect func(context.Context, string) (*host.Report, error)) int {
	flags := flag.NewFlagSet("server inspect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "print the inspection report as JSON")
	timeout := flags.Duration("timeout", 15*time.Second, "overall inspection deadline")
	flags.Usage = func() { writeServerHelp(flags.Output(), "inspect", flags) }
	if err := parseFlags(flags, args, stdout, stderr); err != nil {
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
		return lifecycleError(stderr, err)
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			fmt.Fprintf(stderr, "error: write report: %v\n", err)
			return 1
		}
	} else if p := presentationFor(stdout); p != nil {
		if err := p.write(stdout, p.renderServerInspect(target, report)); err != nil {
			return lifecycleError(stderr, fmt.Errorf("write report: %w", err))
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
