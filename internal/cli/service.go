package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
)

const serviceHelp = `Usage:
  soar service bind NAME --app APP --server SERVER [flags]
  soar service remove NAME [flags]
Bind a service locally or preview/remove a service from the selected app.
Run soar service bind --help or soar service remove --help for options.
`

func serviceCommand(ctx context.Context, args []string, stdout, stderr io.Writer, deps appDeps) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Fprint(stdout, serviceHelp)
		return 0
	}
	if len(args) < 2 || (args[0] != "bind" && args[0] != "remove") {
		return usageError(stderr, "usage: soar service bind|remove NAME --app APP --server SERVER [--yes] [--allow-protected]")
	}
	// Accept the documented name-first syntax; appCommand retains flags-first parsing.
	rest := args[1:]
	if !strings.HasPrefix(rest[0], "-") {
		rest = append([]string{"--service", rest[0]}, rest[1:]...)
	}
	return appCommand(ctx, "service-"+args[0], rest, stdout, stderr, deps)
}
