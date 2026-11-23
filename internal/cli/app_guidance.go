package cli

import (
	"flag"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
)

// Suggested commands target POSIX shells, or PowerShell on Windows. Do not let
// spaces or shell metacharacters in local paths change their argument boundaries.
func suggestedCommand(args ...string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		safe := arg != "" && strings.IndexFunc(arg, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_./:=+-", r))
		}) < 0
		if safe {
			quoted[i] = arg
			continue
		}
		escape := "'\\''"
		if runtime.GOOS == "windows" {
			escape = "''"
		}
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", escape) + "'"
	}
	return strings.Join(quoted, " ")
}

func deploymentApproval(flags *flag.FlagSet, manifest, server, service string) string {
	args := []string{"vamos", "deploy", "--service", service, "--yes", "-f", manifest, "--server", server}
	args = appendSuppliedFlags(args, flags, "supersede", "executor", "timeout", "reuse-data")
	return suggestedCommand(args...)
}

// Captured-state operations do not read new deployment inputs. Pin both
// identities instead of allowing a later manifest edit to retarget approval.
func capturedApproval(flags *flag.FlagSet, action, server, app string) string {
	args := []string{"vamos", action}
	switch action {
	case "cleanup", "destroy", "rollback":
		args = appendSuppliedFlags(args, flags, "service")
	case "service-remove":
		args = []string{"vamos", "service", "remove", flags.Lookup("service").Value.String()}
	default:
		return ""
	}
	args = append(args, "--server", server, "--app", app, "--yes")
	args = appendSuppliedFlags(args, flags, "executor", "timeout", "allow-protected", "allow-downtime")
	return suggestedCommand(args...)
}

func appendSuppliedFlags(args []string, flags *flag.FlagSet, names ...string) []string {
	flags.Visit(func(f *flag.Flag) {
		if !slices.Contains(names, f.Name) {
			return
		}
		if boolean, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			args = append(args, "--"+f.Name+"="+f.Value.String())
		} else {
			args = append(args, "--"+f.Name, f.Value.String())
		}
	})
	return args
}

func configRecoveryHint(w io.Writer, action string, flags *flag.FlagSet) {
	// Deploy and run require new local inputs. Never suggest a bypass for them.
	switch action {
	case "status", "jobs", "logs", "resume", "rollback", "cleanup", "destroy", "service-bind", "service-remove":
	default:
		return
	}
	args := []string{"vamos", action}
	if action == "service-bind" || action == "service-remove" {
		args = []string{"vamos", "service", strings.TrimPrefix(action, "service-"), flags.Lookup("service").Value.String()}
	}
	server := flags.Lookup("server").Value.String()
	if server == "" {
		server = "SERVER"
	}
	args = append(args, "--server", server, "--app", "APP")
	// Preserve observation scope, never carry automatic destructive approval
	// into a hint that still requires the operator to supply an app identity.
	args = appendSuppliedFlags(args, flags, "run", "candidate", "follow", "tail", "timeout", "executor")
	if action != "service-bind" && action != "service-remove" {
		args = appendSuppliedFlags(args, flags, "service")
	}
	if action == "resume" {
		args = append(args, flags.Arg(0))
	}
	fmt.Fprintf(w, "Local configuration could not be loaded. Supply the intended server and app explicitly (replace any SERVER/APP placeholders):\n  %s\n", suggestedCommand(args...))
	if action == "service-bind" {
		fmt.Fprintln(w, "With --app, binding writes to the current directory; repair the manifest instead to bind its directory.")
	}
}
