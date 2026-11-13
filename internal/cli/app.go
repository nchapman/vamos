package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/nchapman/soar/internal/buildcontext"
	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/host"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/registry"
	"github.com/nchapman/soar/internal/ssh"
)

type appDeps struct {
	Registry func() (*registry.Store, error)
	Archive  func(string, config.Build) ([]byte, error)
	Inspect  func(context.Context, string) (*host.Report, error)
	Artifact func(string, string) ([]byte, error)
	Run      func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error)
	Stream   func(context.Context, string, deploy.SessionRequest, []byte, io.Reader, io.Writer, io.Writer) (int, error)
}

func defaultAppDeps() appDeps {
	return appDeps{Registry: registry.Default, Archive: buildcontext.Archive, Inspect: ssh.Inspect, Artifact: ssh.Artifact, Run: ssh.RunApp, Stream: ssh.StreamApp}
}

func appCommand(ctx context.Context, action string, args []string, stdout, stderr io.Writer, deps appDeps) int {
	flags := flag.NewFlagSet(action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	filename := flags.String("f", config.DefaultFile, "app configuration")
	serverFlag := flags.String("server", "", "registered server override")
	appFlag := flags.String("app", "", "app name for operations outside a project")
	executorFile := flags.String("executor", "", "Linux executor artifact override")
	service := flags.String("service", "", "service selector")
	runOperation := flags.String("run", "", "captured run operation (status/logs only)")
	candidate := flags.Bool("candidate", false, "candidate logs (logs only)")
	follow := flags.Bool("follow", false, "stream selected container logs until disconnect (logs only; -f remains config file)")
	supersede := flags.String("supersede", "", "replace a stopped pre-promotion operation with new inputs")
	reuseData := flags.Bool("reuse-data", false, "authorize exact retained volume reuse after destroy (deploy only)")
	yes := flags.Bool("yes", false, "approve the selected update or apply a previewed operation")
	allowDowntime := flags.Bool("allow-downtime", false, "authorize stop/start rollback (rollback only)")
	reAdd := flags.Bool("re-add", false, "re-create a removed service (requires --service and --yes)")
	allowProtected := flags.Bool("allow-protected", false, "approve removal of a manual service")
	maintenance := action == "cleanup" || action == "destroy" || action == "service-remove"
	asJSON := flags.Bool("json", false, "print JSON instead of human output")
	tail := flags.Int("tail", 100, "log tail lines (1-1000, output also byte bounded)")
	mutation := action == "deploy" || action == "run" || action == "resume" || action == "rollback" || maintenance
	deadline := 15 * time.Second
	if mutation {
		deadline = 25 * time.Minute
	}
	timeout := flags.Duration("timeout", deadline, "client deadline; started mutations survive disconnection")
	flags.Usage = func() { writeAppHelp(flags.Output(), action, flags) }
	if err := parseFlags(flags, args, stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if (action == "service-bind" || action == "service-remove") && *service == "" {
		return usageError(stderr, "service bind/remove requires a service name")
	}
	if *reAdd && (action != "deploy" || *service == "" || !*yes) {
		return usageError(stderr, "--re-add requires deploy --service NAME --yes")
	}
	if *allowProtected && action != "service-remove" {
		return usageError(stderr, "--allow-protected is service remove only")
	}
	yesProvided := false
	reuseDataProvided := false
	allowDowntimeProvided := false
	serviceProvided, candidateProvided, runProvided := false, false, false
	followProvided, timeoutProvided := false, false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "follow" {
			followProvided = true
		}
		if f.Name == "timeout" {
			timeoutProvided = true
		}
		if f.Name == "allow-downtime" {
			allowDowntimeProvided = true
		}
		if f.Name == "run" {
			runProvided = true
		}
		if f.Name == "reuse-data" {
			reuseDataProvided = true
		}
		if f.Name == "service" {
			serviceProvided = true
		}
		if f.Name == "candidate" {
			candidateProvided = true
		}
		if f.Name == "yes" {
			yesProvided = true
		}
	})
	if (serviceProvided && (action != "status" && action != "logs" && action != "deploy" && action != "run" && action != "rollback" && action != "service-remove" && action != "service-bind" || *service == "")) || (candidateProvided && action != "logs") {
		return usageError(stderr, "--service requires deploy/run/status/logs/rollback; --candidate is logs only")
	}
	if runProvided && (*runOperation == "" || (action != "status" && action != "logs") || serviceProvided || candidateProvided) {
		return usageError(stderr, "--run is status/logs only and cannot be combined with --service or --candidate")
	}
	if action == "run" && (*service == "" || flags.NArg() == 0) {
		return usageError(stderr, "run requires --service and a nonempty command after --")
	}
	if allowDowntimeProvided && action != "rollback" {
		return usageError(stderr, "--allow-downtime is rollback only")
	}
	if reuseDataProvided && action != "deploy" {
		return usageError(stderr, "--reuse-data is deploy only")
	}
	if followProvided && action != "logs" {
		return usageError(stderr, "--follow is logs only")
	}
	if *follow && runProvided {
		return usageError(stderr, "--follow cannot be combined with --run; recorded run/build diagnostics are bounded snapshots")
	}
	if *follow && !timeoutProvided {
		*timeout = 0
	}
	if *timeout < 0 || (*timeout == 0 && !*follow) || *tail < 1 || *tail > 1000 {
		return usageError(stderr, "invalid timeout, service, or log tail")
	}
	if (action == "resume" && flags.NArg() != 1) || (action != "resume" && action != "run" && flags.NArg() != 0) {
		return usageError(stderr, "resume requires one operation ID; other app commands have no positional arguments")
	}
	if (*supersede != "" && action != "deploy") || (*asJSON && action != "status" && !maintenance) || (yesProvided && !maintenance && action != "rollback" && !(action == "deploy" && *service != "")) || (action != "logs" && *tail != 100) {
		return usageError(stderr, "option does not apply to this app command")
	}
	if *appFlag != "" && (action == "deploy" || action == "run" || *serverFlag == "") {
		return usageError(stderr, "--app is for status/logs/resume/cleanup/destroy/rollback and requires an explicit --server")
	}
	name := *appFlag
	var app *config.App
	var dir, manifestPath string
	if name == "" {
		var err error
		app, err = config.Load(*filename)
		if err != nil {
			code := lifecycleError(stderr, err)
			configRecoveryHint(stderr, action, flags)
			return code
		}
		name = app.Name
		absolute, err := filepath.Abs(*filename)
		if err != nil {
			return lifecycleError(stderr, err)
		}
		manifestPath = absolute
		dir = filepath.Dir(absolute)
	}
	if err := lifecycle.ValidateConfig(lifecycle.Config{Name: name, Domain: "validation.invalid"}); err != nil {
		return usageError(stderr, "invalid app name")
	}
	if action == "deploy" || action == "run" {
		if err := deploy.ValidateApp(app); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	store, err := deps.Registry()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	local, err := store.Read()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	server, err := selectAppServer(local, dir, name, *serverFlag)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	request := deploy.Request{Version: 1, Action: action, App: name, Host: lifecycle.Config{Name: server.Name, Domain: server.Domain}}
	if dir == "" && action == "service-bind" {
		dir, err = os.Getwd()
		if err != nil {
			return lifecycleError(stderr, err)
		}
	}
	request.Apply = *yes
	request.AllowProtected = *allowProtected
	request.ReAdd = *reAdd
	request.Partial = action == "deploy"
	if action == "service-bind" {
		request.Action = "status"
	}
	var selectedNames []string
	request.AllowDowntime = *allowDowntime
	request.Service = *service
	request.Candidate = *candidate
	request.RunOperation = *runOperation
	if action == "run" {
		request.Command = flags.Args()
	}
	var archive []byte
	if maintenance || action == "rollback" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return lifecycleError(stderr, err)
		}
		request.Operation = hex.EncodeToString(id[:])
	}
	if action == "deploy" || action == "run" {
		request.Config = app
		request.Supersede = *supersede
		request.ReuseData = *reuseData
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return lifecycleError(stderr, err)
		}
		request.Operation = hex.EncodeToString(id[:])
		request.ContextFormat = "bundle-v1"
		selected, selectErr := buildcontext.Select(app, *service)
		if selectErr != nil {
			return lifecycleError(stderr, selectErr)
		}
		if action == "deploy" {
			bindings, e := readServiceBindings(dir, server.Target, name)
			if e != nil {
				return lifecycleError(stderr, e)
			}
			request.Bindings = map[string]string{}
			for n := range selected.Services {
				selectedNames = append(selectedNames, n)
				if b, ok := bindings[n]; ok {
					if request.HostIdentity != "" && request.HostIdentity != b.HostIdentity {
						return lifecycleError(stderr, errors.New("selected bindings have different host identities; explicitly bind again"))
					}
					request.HostIdentity = b.HostIdentity
					request.Bindings[n] = b.Incarnation
				}
			}
			sort.Strings(selectedNames)
		}
		archive, err = buildcontext.Bundle(*filename, selected, deps.Archive)
		if err != nil {
			return lifecycleError(stderr, err)
		}
	} else if action == "resume" {
		request.Operation = flags.Arg(0)
	} else if action == "logs" {
		request.Tail = *tail
	}
	if err := deploy.ValidateRequest(request); err != nil {
		return usageError(stderr, err.Error())
	}
	if err := ssh.ValidateTarget(server.Target); err != nil {
		return usageError(stderr, err.Error())
	}
	if dir != "" && *serverFlag == "" && action != "service-bind" {
		if err := bindProject(dir, name, server); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	var binary []byte
	if action == "deploy" && *service == "" {
		if _, err := fmt.Fprintln(stdout, "Declared-service deploy: private services use stop/start replacement; a brief interruption is expected."); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	warningApp := app
	if app != nil && *service == "" && action == "deploy" {
		copyApp := *app
		copyApp.Services = make(map[string]config.Service)
		for n, s := range app.Services {
			if s.Update != config.UpdateManual {
				copyApp.Services[n] = s
			}
		}
		warningApp = &copyApp
	}
	if app != nil && *service != "" {
		warningApp, _ = buildcontext.Select(app, *service)
	}
	if err := printVolumeWarnings(stdout, action, *supersede, warningApp); err != nil {
		return lifecycleError(stderr, err)
	}
	if mutation && !*asJSON {
		if _, err := fmt.Fprintf(stdout, "%s / %s: %s (operation %s)\n", server.Name, name, action, request.Operation); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	{
		inspectCtx, stop := context.WithTimeout(ctx, 15*time.Second)
		report, err := deps.Inspect(inspectCtx, server.Target)
		stop()
		if err != nil {
			return lifecycleError(stderr, err)
		}
		if !report.Supported || !report.Root {
			return lifecycleError(stderr, errors.New("deployment requires root on a supported Ubuntu host"))
		}
		binary, err = deps.Artifact(*executorFile, report.Architecture)
		if err != nil {
			return lifecycleError(stderr, err)
		}
	}
	if action == "deploy" {
		fmt.Fprintf(stdout, "Target %s (%s) / %s; selected services: %v\n", server.Name, server.Target, name, selectedNames)
	}
	if (action == "service-bind" || action == "service-remove") && !*asJSON {
		fmt.Fprintf(stdout, "Target %s (%s) / %s; selected service: %s\n", server.Name, server.Target, name, *service)
	}
	if *follow {
		sessionRequest := deploy.SessionRequest{Version: 1, Action: "logs", App: name, Host: request.Host, Service: *service, Candidate: *candidate, Tail: *tail}
		return runSession(ctx, server.Target, sessionRequest, binary, nil, stdout, stderr, deps)
	}
	response, err := deps.Run(ctx, server.Target, request, binary, archive)
	if action == "deploy" {
		updates := acknowledgedBindings(request, response.Status, selectedNames)
		if len(updates) > 0 {
			if e := writeServiceBindings(dir, server.Target, name, updates); e != nil {
				fmt.Fprintf(stderr, "Remote operation %s may have completed; inspect status before retrying. Local binding could not be saved.\n", request.Operation)
				return lifecycleError(stderr, e)
			}
		}
	}
	if err != nil {
		if action == "deploy" && response.Version == 1 && response.Error != "" && response.ErrorCode == deploy.CodeBindingRequired {
			selected := "NAME"
			if len(selectedNames) == 1 {
				selected = selectedNames[0]
			}
			bindingArgs := []string{"soar", "service", "bind", selected, "-f", manifestPath, "--server", server.Name}
			bindingArgs = appendSuppliedFlags(bindingArgs, flags, "executor", "timeout")
			fmt.Fprintf(stderr, "Inspect status first; explicitly adopt only the intended service:\n  %s\n", suggestedCommand(bindingArgs...))
		}
		code := lifecycleError(stderr, err)
		if action == "run" {
			fmt.Fprintf(stderr, "Inspect: soar status --run %s --server %s --app %s\n", request.Operation, server.Name, name)
		} else if mutation {
			fmt.Fprintf(stderr, "Inspect: soar status --server %s --app %s\nIf recorded, resume: soar resume --server %s --app %s %s\n", server.Name, name, server.Name, name, request.Operation)
		}
		return code
	}
	if action == "service-bind" {
		if response.Status == nil || response.Status.HostIdentity == "" || response.Status.Services[*service].Incarnation == "" {
			return lifecycleError(stderr, errors.New("status has no service/host identity; refusing to bind"))
		}
		if err := writeServiceBindings(dir, server.Target, name, serviceBindings{*service: {response.Status.HostIdentity, response.Status.Services[*service].Incarnation}}); err != nil {
			return lifecycleError(stderr, err)
		}
		fmt.Fprintln(stdout, "Service bound locally; no deployment performed.")
		return 0
	}
	if p := response.DeployPreview; p != nil {
		if _, err := fmt.Fprintf(stdout, "Deploy service=%q current-image=%q desired-image=%q downtime=%t\n", p.Service, p.CurrentImage, p.DesiredImage, p.Downtime); err != nil {
			return lifecycleError(stderr, err)
		}
		if p.NeedsConfirmation && !request.Apply {
			if _, err := fmt.Fprintf(stdout, "Approve: %s\n", deploymentApproval(flags, manifestPath, server.Name, p.Service)); err != nil {
				return lifecycleError(stderr, err)
			}
			return 0
		}
	}
	if p := response.RollbackPreview; p != nil {
		if err := printRollbackPreview(stdout, p); err != nil {
			return lifecycleError(stderr, err)
		}
		if !request.Apply {
			return 0
		}
	}
	result := response.Run
	if result == nil && response.Status != nil {
		result = response.Status.Run
	}
	if result != nil && !*asJSON {
		if _, err := fmt.Fprintf(stdout, "Run: operation=%q service=%q phase=%q container=%q image=%q\n", result.Operation, result.Service, result.Phase, result.Container, result.Image); err != nil {
			return lifecycleError(stderr, err)
		}
		if _, err := fmt.Fprint(stdout, result.Output); err != nil {
			return lifecycleError(stderr, err)
		}
		if result.Error != "" {
			fmt.Fprintf(stderr, "Run error: %q\n", result.Error)
		}
		if result.ExitCode != nil {
			if _, err := fmt.Fprintf(stdout, "Run exit code: %d\n", *result.ExitCode); err != nil {
				return lifecycleError(stderr, err)
			}
			if action == "run" {
				return *result.ExitCode
			}
		}
	}
	if action == "run" {
		fmt.Fprintf(stderr, "Run outcome unknown. Inspect: soar status --run %s --server %s --app %s\n", request.Operation, server.Name, name)
		return 1
	}
	if action == "logs" {
		if response.LogService != "" || response.LogContainer != "" {
			if _, err := fmt.Fprintf(stderr, "Logs: service=%q container=%q candidate=%t\n", response.LogService, response.LogContainer, response.LogCandidate); err != nil {
				return lifecycleError(stderr, err)
			}
		}
		if _, err := io.WriteString(stdout, response.Logs); err != nil {
			return lifecycleError(stderr, err)
		}
		return 0
	}
	if action == "status" && *asJSON {
		if response.Status == nil {
			return lifecycleError(stderr, errors.New("remote response has no app status"))
		}
		if err := json.NewEncoder(stdout).Encode(response.Status); err != nil {
			return lifecycleError(stderr, err)
		}
		return 0
	}
	if response.Status != nil && !*asJSON {
		for _, volume := range response.Status.Volumes {
			if *service != "" && volume.Service != *service {
				continue
			}
			if _, err := fmt.Fprintf(stdout, "Volume: service=%q name=%q mount=%q id=%q state=%q missing=%t data-may-have-changed=%t\n", volume.Service, volume.Name, volume.Mount, volume.ID, volume.State, volume.Missing, volume.DataMayHaveChanged); err != nil {
				return lifecycleError(stderr, err)
			}
		}
		for _, secret := range response.Status.Secrets {
			if len(secret.AppliedVersions) > 0 {
				if _, err := fmt.Fprintf(stdout, "Secret %s applied by service: %v\n", secret.Name, secret.AppliedVersions); err != nil {
					return lifecycleError(stderr, err)
				}
			}
			if _, err := fmt.Fprintf(stdout, "Secret %s: staged=%s applied=%s pending=%t services=%v\n", secret.Name, secret.StagedVersion, secret.AppliedVersion, secret.Pending, secret.Services); err != nil {
				return lifecycleError(stderr, err)
			}
		}
		if response.Status.Phase == "staged" {
			fmt.Fprintln(stdout, "No deployment recorded; staged secrets are retained.")
			return 0
		}
	}
	if maintenance || response.Plan != nil || (response.Status != nil && response.Status.Phase == "destroyed") {
		if *asJSON {
			if err := json.NewEncoder(stdout).Encode(response); err != nil {
				return lifecycleError(stderr, err)
			}
			return 0
		}
		message := "Maintenance completed."
		if maintenance && !request.Apply {
			message = "Preview only: no resources deleted."
		} else if action == "destroy" || (response.Status != nil && response.Status.Phase == "destroyed") {
			message = "App destroyed; no live route is configured."
		}
		if _, err := fmt.Fprintln(stdout, message); err != nil {
			return lifecycleError(stderr, err)
		}
		if p := response.Plan; p != nil {
			if _, err := fmt.Fprintf(stdout, "Plan: %s / %s\nContainers: %q\nImages: %q\nArchives: %q\nNetwork: %q\nRetained: %q\n", p.Action, p.App, p.Containers, p.Images, p.Archives, p.Network, p.Retained); err != nil {
				return lifecycleError(stderr, err)
			}
		}
		return 0
	}
	if response.Status == nil {
		return lifecycleError(stderr, errors.New("remote response has no app status"))
	}
	s := response.Status
	if len(s.Services) > 0 {
		if _, err := fmt.Fprintf(stdout, "%s / %s: phase=%q operation=%q host-operation-active=%t\n", server.Name, name, s.Phase, s.Operation, s.Active); err != nil {
			return lifecycleError(stderr, err)
		}
		names := make([]string, 0, len(s.Services))
		hasPublic := false
		for name, service := range s.Services {
			names = append(names, name)
			hasPublic = hasPublic || service.Kind == "public"
		}
		sort.Strings(names)
		for _, serviceName := range names {
			if *service != "" && serviceName != *service {
				continue
			}
			ss := s.Services[serviceName]
			if err := printDomainStatuses(stdout, ss.Domains); err != nil {
				return lifecycleError(stderr, err)
			}
			if ss.Candidate != "" {
				if _, err := fmt.Fprintf(stdout, "%s candidate: %q\n", serviceName, ss.Candidate); err != nil {
					return lifecycleError(stderr, err)
				}
			}
			if _, err := fmt.Fprintf(stdout, "%s / %s / %s: kind=%q phase=%q running=%t available=%t unavailable=%t data-may-have-changed=%t\nURL: %s\nOperation: %q\nContainer: %q\nImage: %q\n", server.Name, name, serviceName, ss.Kind, ss.Phase, ss.Running, ss.Available, ss.Unavailable, ss.DataMayHaveChanged, ss.URL, ss.Operation, ss.Container, ss.Image); err != nil {
				return lifecycleError(stderr, err)
			}
		}
		if s.LastError != "" {
			fmt.Fprintf(stdout, "Last error: %q\n", s.LastError)
		}
		if mutation {
			message := "Private-only app: no public route is configured."
			if hasPublic {
				message = "Routes configured. Public DNS and certificate readiness are not implied by deployment success."
			}
			if _, err := fmt.Fprintln(stdout, message); err != nil {
				return lifecycleError(stderr, err)
			}
		}
		return 0
	}
	if _, err := fmt.Fprintf(stdout, "%s / %s / web: phase=%q running=%t host-operation-active=%t\nURL: https://%s.%s\nOperation: %q\nContainer: %q\n", server.Name, name, s.Phase, s.Running, s.Active, name, server.Domain, s.Operation, s.Container); err != nil {
		return lifecycleError(stderr, err)
	}
	if s.LastError != "" {
		if _, err := fmt.Fprintf(stdout, "Last error: %q\n", s.LastError); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	if mutation {
		if _, err := fmt.Fprintln(stdout, "Route configured. Public DNS and certificate readiness are not implied by deployment success."); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	return 0
}

// Resume uses captured remote inputs, never the possibly edited local config.
func printVolumeWarnings(out io.Writer, action, supersede string, app *config.App) error {
	if action == "deploy" && app != nil {
		names := make([]string, 0, len(app.Services))
		for name := range app.Services {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			service := app.Services[name]
			if service.Volume == nil {
				continue
			}
			if _, err := fmt.Fprintf(out, "Volume: service=%q name=%q mount=%q (identity resolved remotely). Replacement uses stop/start", name, service.Volume.Name, service.Volume.Mount); err != nil {
				return err
			}
			if service.HTTP != nil {
				if _, err := fmt.Fprint(out, "; public requests receive HTTP 503 during replacement"); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(out, ". Data is retained. Starting new code may change it; no automatic rollback."); err != nil {
				return err
			}
		}
	}
	if action == "resume" || (action == "deploy" && supersede != "") {
		_, err := fmt.Fprintln(out, "If the captured operation uses volumes: data is retained and may already have changed; starting new code may change it further. No automatic rollback. Code must tolerate current data. Volume-backed replacement uses stop/start, with HTTP 503 for public services.")
		return err
	}
	return nil
}
