package cli

import (
	"context"
	"io"
	"strings"
)

func serviceCommand(ctx context.Context, args []string, stdout, stderr io.Writer, deps appDeps) int {
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
