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
		fmt.Fprintln(stderr, "Usage: soar apps [--server NAME] [--json] [--executor FILE] [--timeout duration]")
	}
	if err := flags.Parse(args); err != nil {
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
	inspectCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	report, err := deps.Inspect(inspectCtx, server.Target)
	stop()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if report == nil || !report.Supported || !report.Root {
		return lifecycleError(stderr, errors.New("apps requires root on a supported Ubuntu host"))
	}
	binary, err := deps.Artifact(*executor, report.Architecture)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	response, err := deps.Run(ctx, server.Target, request, binary, nil)
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
