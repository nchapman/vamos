package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/ssh"
)

func appsCommand(ctx context.Context, args []string, stdout, stderr io.Writer, deps appDeps) int {
	flags := flag.NewFlagSet("apps", flag.ContinueOnError)
	flags.SetOutput(stderr)
	serverName := flags.String("server", "", "registered server (default: registered default)")
	asJSON := flags.Bool("json", false, "JSON inventory")
	executor := flags.String("executor", "", "Linux executor artifact override")
	timeout := flags.Duration("timeout", 15*time.Second, "client deadline")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: soar apps [--server NAME] [--json] [--executor FILE] [--timeout duration]")
		fmt.Fprintln(flags.Output(), "Uses the registered default server unless --server is supplied; ignores project binding.")
		fmt.Fprintln(flags.Output(), "Lists recorded inventory, not live health.")
	}
	if err := parseFlags(flags, args, stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *timeout <= 0 {
		return usageError(stderr, "apps accepts no positional arguments and requires a positive timeout")
	}
	store, err := deps.Registry()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	state, err := store.Read()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	server, err := selectAppServer(state, "", "", *serverName)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	request := deploy.Request{Version: 1, Action: "apps", Host: lifecycle.Config{Name: server.Name, Domain: server.Domain}}
	if err := deploy.ValidateRequest(request); err != nil {
		return lifecycleError(stderr, err)
	}
	if err := ssh.ValidateTarget(server.Target); err != nil {
		return lifecycleError(stderr, err)
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	response, err := runAppTransport(ctx, server.Target, *executor, request, nil, deps)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if response.Version != 1 || response.Error != "" || response.Apps == nil {
		return lifecycleError(stderr, errors.New("invalid apps response"))
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(response.Apps); err != nil {
			return lifecycleError(stderr, err)
		}
		return 0
	}
	if p := presentationFor(stdout); p != nil {
		if err := p.write(stdout, p.renderApps(server.Name, server.Target, response.Apps)); err != nil {
			return lifecycleError(stderr, err)
		}
		return 0
	}
	if _, err := fmt.Fprintf(stdout, "%s (%s): recorded inventory (not live health).\n", server.Name, server.Target); err != nil {
		return lifecycleError(stderr, err)
	}
	if len(response.Apps.Entries) == 0 {
		_, err = fmt.Fprintf(stdout, "%s: no registered apps.\n", server.Name)
	} else {
		_, err = fmt.Fprintln(stdout, "APP\tPHASE\tSERVICES (COUNT)\tRETAINED SECRETS\tVOLUME RECORDS\tRETAINED VOLUMES")
		for _, entry := range response.Apps.Entries {
			if err != nil {
				break
			}
			_, err = fmt.Fprintf(stdout, "%s\t%s\t%s (%d)\t%t\t%d\t%t\n", entry.App, entry.Phase, strings.Join(entry.Services, ","), entry.ServiceCount, entry.RetainedSecrets, entry.VolumeRecords, entry.RetainedVolumes)
		}
	}
	if err != nil {
		return lifecycleError(stderr, err)
	}
	return 0
}

func (p *presentation) renderApps(server, target string, inventory *deploy.AppList) string {
	text := p.header("apps", "", server, target)
	text += p.prose("Recorded inventory (not live health).")
	text += p.field("Apps", fmt.Sprint(len(inventory.Entries)))
	if len(inventory.Entries) == 0 {
		return text + p.section("No registered apps")
	}

	headings := []string{"APP", "PHASE", "SERVICES (COUNT)", "RETAINED SECRETS", "VOLUME RECORDS", "RETAINED VOLUMES"}
	widths := make([]int, len(headings))
	for i, heading := range headings {
		widths[i] = ansi.StringWidth(heading)
	}
	rows := make([][]string, 0, len(inventory.Entries))
	for _, entry := range inventory.Entries {
		row := []string{
			entry.App, entry.Phase, fmt.Sprintf("%s (%d)", strings.Join(entry.Services, ","), entry.ServiceCount),
			yesNo(entry.RetainedSecrets), fmt.Sprint(entry.VolumeRecords), yesNo(entry.RetainedVolumes),
		}
		for i, value := range row {
			widths[i] = max(widths[i], ansi.StringWidth(displayText(value)))
		}
		rows = append(rows, row)
	}
	tableWidth := 2 * (len(headings) - 1)
	for _, width := range widths {
		tableWidth += width
	}
	// Use the table only when every complete, escaped value fits. Terminal
	// soft wrapping in records preserves long names instead of truncating them.
	if tableWidth <= p.width {
		text += "\n" + p.appsTableRow(headings, widths, true)
		for _, row := range rows {
			text += p.appsTableRow(row, widths, false)
		}
		return text
	}
	for _, entry := range inventory.Entries {
		text += p.section(entry.App)
		text += p.field("Phase", entry.Phase)
		text += p.field("Services", strings.Join(entry.Services, ","))
		text += p.field("Service count", fmt.Sprint(entry.ServiceCount))
		text += p.field("Retained secrets", yesNo(entry.RetainedSecrets))
		text += p.field("Volume records", fmt.Sprint(entry.VolumeRecords))
		text += p.field("Retained volumes", yesNo(entry.RetainedVolumes))
	}
	return text
}

func (p *presentation) appsTableRow(values []string, widths []int, heading bool) string {
	var row strings.Builder
	for i, value := range values {
		role := ""
		if heading {
			role = "secondary"
		}
		row.WriteString(p.ink(role, value, false))
		if i < len(values)-1 {
			row.WriteString(strings.Repeat(" ", widths[i]-ansi.StringWidth(displayText(value))+2))
		}
	}
	row.WriteByte('\n')
	return row.String()
}
