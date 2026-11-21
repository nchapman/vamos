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
	"github.com/nchapman/vamos/internal/deploy"
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
	return showServerApps(ctx, stdout, stderr, deps, server, inventoryOptions{executor: executor, timeout: timeout, asJSON: asJSON, statusHints: true})
}

func (p *presentation) serverOverview(servers []registry.Server, defaultName string) string {
	var text strings.Builder
	text.WriteString(p.header("servers", "", "", ""))
	if len(servers) == 0 {
		text.WriteString(p.section("No registered servers"))
		text.WriteString(p.prose("Connect an existing host or add a new one:"))
		text.WriteString(p.command("vamos help server connect"))
		text.WriteString(p.command("vamos help server add"))
		return text.String()
	}
	text.WriteString(p.prose("Locally registered servers."))
	for _, server := range servers {
		label := server.Name
		if label == defaultName {
			label += " (default)"
		}
		text.WriteString(p.section(label))
		text.WriteString(p.field("SSH target", server.Target))
		text.WriteString(p.field("Domain", server.Domain))
	}
	text.WriteString(p.section("List apps on a server"))
	text.WriteString(p.command(suggestedCommand("vamos", "status", "--server", defaultName)))
	return text.String()
}

func (p *presentation) appStatusHint(server string, apps *deploy.AppList) string {
	if len(apps.Entries) == 0 {
		return ""
	}
	return p.section("Inspect app status") + p.command(suggestedCommand("vamos", "status", "--server", server, "--app", apps.Entries[0].App))
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
	} else if p := presentationFor(stdout); p != nil {
		err = p.write(stdout, p.serverOverview(servers, state.Default))
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
