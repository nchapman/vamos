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
		usage = "soar server inspect [OPTIONS] TARGET"
		notes = "Inspect a host read-only, without provisioning or registering it."
		names = append(names, "json")
	case "add":
		usage = "soar server add [OPTIONS] --yes --domain DOMAIN NAME TARGET"
		notes = "Bootstrap a dedicated supported Ubuntu host as root. Requires explicit --yes.\nDNS is configured separately. Started mutations survive client disconnection."
		names = append(names, "yes", "domain", "executor")
	case "connect":
		usage = "soar server connect [OPTIONS] --domain DOMAIN NAME TARGET"
		notes = "Verify an existing ready installation and register it locally. Requires root SSH.\nNo bootstrap, executor upload, app changes or project binding is performed.\nNAME and DOMAIN must match the installed identity; SSH host keys must already be trusted."
		names = append(names, "domain")
	case "status":
		usage = "soar server status [OPTIONS] NAME [TARGET]"
		notes = "Read the host lifecycle journal. TARGET is optional for registered names;\nprovide it to recover an unregistered host."
		names = append(names, "json")
	case "teardown":
		usage = "soar server teardown [OPTIONS] --yes --purge NAME [TARGET]"
		notes = "Remove an unused owned installation as root. Requires --yes and --purge.\nRefuses workloads and changed managed resources; purge deletes owned runtime data and certificates.\nTARGET is optional for registered names. Started mutations survive client disconnection."
		names = append(names, "yes", "purge", "executor")
	}
	fmt.Fprintf(w, "Usage: %s\n\n%s\n\nOptions:\n", usage, notes)
	printFlagDefaults(w, flags, names...)
	fmt.Fprintln(w, "\nTARGET is an SSH alias or [user@]host. Use SSH config for ports, identities and jump hosts.\nAn already trusted host key and noninteractive key/agent authentication are required.\nFlags precede NAME and TARGET.")
}
