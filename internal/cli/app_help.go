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
			"vamos deploy [OPTIONS]", "service preview yes supersede re-add reuse-data",
			"Update automatic services, or only --service NAME. Existing manual services preview unless --yes.\nUse --preview with --service to require a protected preview even if the target changes; it never deploys.\n--yes requires --service; --re-add additionally authorizes recreating a removed service.\n--reuse-data is separate approval for exact retained-volume reuse after destroy.\nPrivate, direct-port, and volume-backed replacements use stop-first (stop/start); application data is not rolled back.\nConfigure direct publishing with services.<name>.ports; direct ports bypass HTTP middleware.\nHTTP middleware switches: services.<name>.http.waf, services.<name>.http.cache, services.<name>.http.compression.",
			"vamos deploy -f api/vamos.yaml --server home --service web",
		},
		"run": {
			"vamos run [OPTIONS] [--service NAME] -- COMMAND [ARG...]", "service",
			"Run captured code noninteractively: no stdin, TTY, implicit shell, or automatic retry.\nDefaults to the sole service when the app defines one; multi-service apps require --service.\nThe remote worker has a 20-minute deadline. Inspect an unknown outcome with status --run, not rerun.",
			"vamos run --service web -- python migrate.py",
		},
		"status": {
			"vamos status [OPTIONS]", "app service run json",
			"Outside a project, list registered servers locally; add --server NAME for its recorded app inventory.\nIn a project, or with --server NAME --app APP, inspect live app status. --run selects a captured\none-off operation and cannot be combined with --service.",
			"vamos status --server home --app notes --json",
		},
		"jobs": {"vamos jobs [show ID] [OPTIONS]", "app service json", "List retained scheduled job runs, or show one run and its retained combined output.", "vamos jobs show ID --server home --app notes"},
		"logs": {
			"vamos logs [OPTIONS]", "app service replica run candidate follow tail",
			"Multi-instance services require --replica ORDINAL (1-based); singleton services default to their sole instance.\nSnapshot logs are byte bounded; --follow streams one pinned container and never retargets replacements.\nSnapshot deadline defaults to 15s; follow defaults to no deadline (explicit --timeout 0 is also unlimited).\n--run diagnostics are snapshots and cannot combine with --follow, --service, --replica, or --candidate.\n-f remains the configuration file. Snapshot workload logs go to stdout; target metadata goes to stderr.",
			"vamos logs --service web --follow --tail 100",
		},
		"resume": {
			"vamos resume [OPTIONS] OPERATION", "app",
			"Resume the existing deployment/maintenance operation using captured inputs, not local edits.\nThis does not retry an unknown one-off run. Flags must precede the operation ID.",
			"vamos resume --server home --app notes OPERATION",
		},
		"rollback": {
			"vamos rollback [OPTIONS]", "app service yes allow-downtime",
			"Preview unless --yes. Use captured images and secret versions, never local source or builds.\n--allow-downtime authorizes stop/start rollback; application data is not rolled back.\nCaptured credentials may have been revoked.",
			"vamos rollback --server home --app notes --service web",
		},
		"cleanup": {
			"vamos cleanup [OPTIONS]", "app yes json",
			"Preview owned-resource cleanup unless --yes. Retained data and secrets are not deleted.\n--json emits the full response; large plans explicitly summarize details with exact totals. Preview success does not mean resources were deleted.",
			"vamos cleanup --server home --app notes --json",
		},
		"destroy": {
			"vamos destroy [OPTIONS]", "app yes json",
			"Preview app destruction unless --yes. Retain data and secret versions; remove live app resources.\n--json emits the full response; large plans explicitly summarize details with exact totals. Preview success does not mean the app was destroyed.",
			"vamos destroy --server home --app notes",
		},
		"service-bind": {
			"vamos service bind NAME [OPTIONS]", "app",
			"Explicitly adopt an existing service identity locally; no deployment is performed.\n-f writes the binding beside that manifest. --app with --server writes it in the current directory.",
			"vamos service bind web -f api/vamos.yaml --server home",
		},
		"service-remove": {
			"vamos service remove NAME [OPTIONS]", "app yes allow-protected json",
			"Preview service removal unless --yes. A manual service also requires --allow-protected.\nRetain data and secret versions. --json emits the full response; large plans explicitly summarize details with exact totals.",
			"vamos service remove worker --server home --app notes",
		},
	}
	h := commands[action]
	fmt.Fprintf(w, "Usage: %s\n\n%s\n\nOptions:\n", h.usage, h.notes)
	names := append([]string{"f", "server", "executor", "timeout"}, strings.Fields(h.flags)...)
	printFlagDefaults(w, parsed, names...)
	fmt.Fprintf(w, "\nExample:\n  %s\n\n", h.example)
	fmt.Fprintln(w, "A project uses its saved server binding, or binds to the selected server (defaulting to the registered default) on first contact.\nOnce bound, --server overrides only this invocation without changing the project binding.")
	if slices.Contains(names, "app") {
		fmt.Fprintln(w, "--app requires --server and avoids loading local configuration.")
	}
	if action != "jobs" && action != "service-bind" && action != "service-remove" {
		fmt.Fprintln(w, "Put flags before positional arguments; use -- before a command's literal arguments.")
	}
}
