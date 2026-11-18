package cli

import (
	"flag"
	"fmt"
	"io"
)

func writeServerHelp(w io.Writer, action string, flags *flag.FlagSet) {
	var usage, notes string
	names := []string{"timeout"}
	switch action {
	case "inspect":
		usage = "vamos server inspect [OPTIONS] TARGET"
		notes = "Inspect a host read-only, without provisioning or registering it."
		names = append(names, "json")
	case "add":
		usage = "vamos server add [OPTIONS] --yes [--firewall=true|false] [--domain DOMAIN] NAME TARGET"
		notes = "Bootstrap a dedicated supported Ubuntu host using root or passwordless sudo. Requires explicit --yes.\nDefaults to <hex-ip>.tryvamos.dev when --domain is omitted. Started mutations survive client disconnection.\nThe deploy key (SSH agent or --deploy-key) is installed as the restricted non-root bridge key;\napp operations then need no root SSH.\nChoose --firewall=true|false explicitly in scripts; interactive stdin prompts if omitted.\nManaged firewall is opt-in automatic dedicated-host protection, including Docker public publishing.\nFalse leaves rules untouched. Unsupported existing firewall configurations are refused."
		names = append(names, "yes", "domain", "executor", "deploy-key", "firewall")
	case "upgrade":
		usage = "vamos server upgrade [OPTIONS] --yes [NAME [TARGET]]"
		notes = serverUpgradeImpact + "\nPlan a maintenance window and confirm with --yes. Local `vamos upgrade` only updates your client bundle.\nNAME defaults to the registered default server; TARGET is optional for registered names.\nStarted mutations survive client disconnection."
		names = append(names, "yes", "executor")
	case "connect":
		usage = "vamos server connect [OPTIONS] [--bridge] [--domain DOMAIN] NAME TARGET"
		notes = "Verify an existing ready installation and register it locally. By default, verify readiness using root or passwordless-sudo SSH.\nWith --bridge, verify only the agent identity and deploy protocol through the restricted deploy key; no admin access is used.\nNo bootstrap, executor upload, app changes or project binding is performed.\nNAME (and optional DOMAIN) must match the installed identity; SSH host keys must already be trusted."
		names = append(names, "bridge", "domain")
	case "status":
		usage = "vamos server status [OPTIONS] [NAME [TARGET]]"
		notes = "Read the host lifecycle journal. NAME defaults to the registered default server;\nTARGET is optional for registered names; provide it to recover an unregistered host."
		names = append(names, "json")
	case "teardown":
		usage = "vamos server teardown [OPTIONS] --yes --purge NAME [TARGET]"
		notes = "Remove an unused owned installation as root. Requires --yes and --purge.\nRefuses workloads and changed managed resources; purge deletes owned runtime data and certificates.\nTARGET is optional for registered names. Started mutations survive client disconnection."
		names = append(names, "yes", "purge", "executor")
	}
	fmt.Fprintf(w, "Usage: %s\n\n%s\n\nOptions:\n", usage, notes)
	printFlagDefaults(w, flags, names...)
	fmt.Fprintln(w, "\nTARGET is an SSH alias or [user@]host. Use SSH config for ports, identities and jump hosts.\nAn already trusted host key and noninteractive key/agent authentication are required.\nFlags precede NAME and TARGET.")
}
