package cli

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// The app parser is shared, but help describes only the selected operation.
func writeAppHelp(w io.Writer, action string, parsed *flag.FlagSet) {
	type help struct{ usage, flags, notes, example string }
	commands := map[string]help{
		"deploy": {
			"soar deploy [OPTIONS]", "service yes supersede re-add reuse-data",
			"Update automatic services, or only --service NAME. Existing manual services preview unless --yes.\n--yes requires --service; --re-add additionally authorizes recreating a removed service.\n--reuse-data is separate approval for exact retained-volume reuse after destroy.\nPrivate and volume-backed replacements use stop/start; application data is not rolled back.",
			"soar deploy -f api/soar.yaml --server home --service web",
		},
		"run": {
			"soar run [OPTIONS] --service NAME -- COMMAND [ARG...]", "service",
			"Run captured code noninteractively: no stdin, TTY, implicit shell, or automatic retry.\nThe remote worker has a 20-minute deadline. Inspect an unknown outcome with status --run, not rerun.",
			"soar run --service web -- python migrate.py",
		},
		"status": {
			"soar status [OPTIONS]", "app service run json",
			"Inspect recorded operation and runtime state. --run selects a captured one-off operation\nand cannot be combined with --service. --json emits the status object, not a response envelope.",
			"soar status --server home --app notes --json",
		},
		"logs": {
			"soar logs [OPTIONS]", "app service run candidate follow tail",
			"Snapshot logs are byte bounded; --follow streams one pinned container and never retargets replacements.\nSnapshot deadline defaults to 15s; follow defaults to no deadline (explicit --timeout 0 is also unlimited).\n--run diagnostics are snapshots and cannot combine with --follow, --service, or --candidate.\n-f remains the configuration file. Snapshot workload logs go to stdout; target metadata goes to stderr.",
			"soar logs --service web --follow --tail 100",
		},
		"resume": {
			"soar resume [OPTIONS] OPERATION", "app",
			"Resume the existing deployment/maintenance operation using captured inputs, not local edits.\nThis does not retry an unknown one-off run. Flags must precede the operation ID.",
			"soar resume --server home --app notes OPERATION",
		},
		"rollback": {
			"soar rollback [OPTIONS]", "app service yes allow-downtime",
			"Preview unless --yes. Use captured images and secret versions, never local source or builds.\n--allow-downtime authorizes stop/start rollback; application data is not rolled back.\nCaptured credentials may have been revoked.",
			"soar rollback --server home --app notes --service web",
		},
		"cleanup": {
			"soar cleanup [OPTIONS]", "app yes json",
			"Preview owned-resource cleanup unless --yes. Retained data and secrets are not deleted.\n--json emits the full response, including the exact plan. Preview success does not mean resources were deleted.",
			"soar cleanup --server home --app notes --json",
		},
		"destroy": {
			"soar destroy [OPTIONS]", "app yes json",
			"Preview app destruction unless --yes. Retain data and secret versions; remove live app resources.\n--json emits the full response, including the exact plan. Preview success does not mean the app was destroyed.",
			"soar destroy --server home --app notes",
		},
		"service-bind": {
			"soar service bind NAME [OPTIONS]", "app",
			"Explicitly adopt an existing service identity locally; no deployment is performed.\n-f writes the binding beside that manifest. --app with --server writes it in the current directory.",
			"soar service bind web -f api/soar.yaml --server home",
		},
		"service-remove": {
			"soar service remove NAME [OPTIONS]", "app yes allow-protected json",
			"Preview service removal unless --yes. A manual service also requires --allow-protected.\nRetain data and secret versions. --json emits the full response, including the exact plan.",
			"soar service remove worker --server home --app notes",
		},
	}
	h := commands[action]
	fmt.Fprintf(w, "Usage: %s\n\n%s\n\nOptions:\n", h.usage, h.notes)
	names := append([]string{"f", "server", "executor", "timeout"}, strings.Fields(h.flags)...)
	printFlagDefaults(w, parsed, names...)
	fmt.Fprintf(w, "\nExample:\n  %s\n\n", h.example)
	fmt.Fprintln(w, "A project uses its saved server binding, or binds to the registered default on first contact.\n--server overrides only this invocation; it never changes the project binding.")
	if slices.Contains(names, "app") {
		fmt.Fprintln(w, "--app requires --server and avoids loading local configuration.")
	}
	if action != "service-bind" && action != "service-remove" {
		fmt.Fprintln(w, "Put flags before positional arguments; use -- before a command's literal arguments.")
	}
}
