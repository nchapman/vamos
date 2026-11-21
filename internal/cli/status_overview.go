package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/registry"
)

// A missing implicit manifest can mean "outside a project". Never broaden an
// app-specific request, or hide an invalid manifest (including a dangling link).
func outsideStatusProject(flags *flag.FlagSet, loadErr error) bool {
	if !errors.Is(loadErr, os.ErrNotExist) {
		return false
	}
	eligible := true
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "json", "server", "timeout", "executor":
		default:
			eligible = false
		}
	})
	if !eligible {
		return false
	}
	_, err := os.Lstat(config.DefaultFile)
	return errors.Is(err, os.ErrNotExist)
}

func statusOverview(ctx context.Context, stdout, stderr io.Writer, deps appDeps, server, executor string, timeout time.Duration, asJSON bool) int {
	if server == "" {
		if executor != "" {
			return usageError(stderr, "--executor requires a server or app for status")
		}
		return showServerOverview(stdout, stderr, deps, asJSON)
	}
	code := showServerApps(ctx, stdout, stderr, deps, server, executor, timeout, asJSON)
	if code == 0 && !asJSON {
		if _, err := fmt.Fprintf(stdout, "\nInspect live app status (replace APP):\n  vamos status --server %s --app APP\n", server); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	return code
}

func showServerOverview(stdout, stderr io.Writer, deps appDeps, asJSON bool) int {
	store, err := deps.Registry()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	state, err := store.Read()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	servers := make([]registry.Server, 0, len(state.Servers))
	for _, name := range slices.Sorted(maps.Keys(state.Servers)) {
		servers = append(servers, state.Servers[name])
	}
	if asJSON {
		err = json.NewEncoder(stdout).Encode(struct {
			Default string            `json:"default"`
			Servers []registry.Server `json:"servers"`
		}{state.Default, servers})
	} else {
		var text strings.Builder
		if len(servers) == 0 {
			text.WriteString("No registered servers. Use 'vamos server connect' for an existing host or 'vamos server add' for a new one.\n")
		} else {
			text.WriteString("Registered servers (local configuration):\n")
			for _, server := range servers {
				label := server.Name
				if label == state.Default {
					label += " (default)"
				}
				fmt.Fprintf(&text, "  %s  %s  %s\n", label, displayText(server.Target), displayText(server.Domain))
			}
			fmt.Fprintf(&text, "\nList apps on a server:\n  vamos status --server %s\n", state.Default)
		}
		_, err = io.WriteString(stdout, text.String())
	}
	if err != nil {
		return lifecycleError(stderr, err)
	}
	return 0
}
