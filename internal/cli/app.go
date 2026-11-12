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

const appHelp = `Usage:
  soar deploy [-f soar.yaml] [--server NAME] [--supersede OPERATION] [--executor FILE]
  soar status [-f soar.yaml] [--json] [--service NAME] [--server NAME --app APP]
  soar logs [-f soar.yaml] [--tail 100] [--service NAME] [--candidate] [--server NAME --app APP]
  soar resume [--server NAME --app APP] OPERATION
  soar cleanup [-f soar.yaml | --server NAME --app APP] [--yes] [--json]
  soar destroy [-f soar.yaml | --server NAME --app APP] [--yes] [--json]

All commands accept --executor FILE. Cleanup/destroy preview unless --yes.
Lifecycle --json emits the full response (including the exact plan).

Deploy updates the whole app. Private services use stop/start replacement.
Flags precede positional arguments. Resume uses captured inputs, not local edits.
Without --server, a project's first contact binds it to the registered default.
An explicit --server overrides only this invocation; it never changes the binding.
`

type appDeps struct {
	Registry func() (*registry.Store, error)
	Archive  func(string, config.Build) ([]byte, error)
	Inspect  func(context.Context, string) (*host.Report, error)
	Artifact func(string, string) ([]byte, error)
	Run      func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error)
}

func defaultAppDeps() appDeps {
	return appDeps{registry.Default, buildcontext.Archive, ssh.Inspect, ssh.Artifact, ssh.RunApp}
}

func appCommand(ctx context.Context, action string, args []string, stdout, stderr io.Writer, deps appDeps) int {
	flags := flag.NewFlagSet(action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	filename := flags.String("f", config.DefaultFile, "app configuration")
	serverFlag := flags.String("server", "", "registered server override")
	appFlag := flags.String("app", "", "app name for operations outside a project")
	executorFile := flags.String("executor", "", "Linux executor artifact override")
	service := flags.String("service", "", "service selector (status/logs only)")
	candidate := flags.Bool("candidate", false, "candidate logs (logs only)")
	supersede := flags.String("supersede", "", "replace a stopped pre-promotion operation with new inputs")
	yes := flags.Bool("yes", false, "apply cleanup or destroy (default: preview)")
	maintenance := action == "cleanup" || action == "destroy"
	asJSON := flags.Bool("json", false, "JSON status")
	tail := flags.Int("tail", 100, "log tail lines (1-1000, output also byte bounded)")
	mutation := action == "deploy" || action == "resume" || maintenance
	deadline := 15 * time.Second
	if mutation {
		deadline = 25 * time.Minute
	}
	timeout := flags.Duration("timeout", deadline, "client deadline; started mutations survive disconnection")
	flags.Usage = func() { fmt.Fprint(flags.Output(), appHelp) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	yesProvided := false
	serviceProvided, candidateProvided := false, false
	flags.Visit(func(f *flag.Flag) {
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
	if (serviceProvided && (action != "status" && action != "logs" || *service == "")) || (candidateProvided && action != "logs") {
		return usageError(stderr, "--service is status/logs only; --candidate is logs only; deploy is whole-app")
	}
	if *timeout <= 0 || *tail < 1 || *tail > 1000 {
		return usageError(stderr, "invalid timeout, service, or log tail")
	}
	if (action == "resume" && flags.NArg() != 1) || (action != "resume" && flags.NArg() != 0) {
		return usageError(stderr, "resume requires one operation ID; other app commands have no positional arguments")
	}
	if (*supersede != "" && action != "deploy") || (*asJSON && action != "status" && !maintenance) || (yesProvided && !maintenance) || (action != "logs" && *tail != 100) {
		return usageError(stderr, "option does not apply to this app command")
	}
	if *appFlag != "" && (action == "deploy" || *serverFlag == "") {
		return usageError(stderr, "--app is for status/logs/resume/cleanup/destroy and requires an explicit --server")
	}
	name := *appFlag
	var app *config.App
	var dir string
	if name == "" {
		var err error
		app, err = config.Load(*filename)
		if err != nil {
			return lifecycleError(stderr, err)
		}
		name = app.Name
		absolute, err := filepath.Abs(*filename)
		if err != nil {
			return lifecycleError(stderr, err)
		}
		dir = filepath.Dir(absolute)
	}
	if err := lifecycle.ValidateConfig(lifecycle.Config{Name: name, Domain: "validation.invalid"}); err != nil {
		return usageError(stderr, "invalid app name")
	}
	if action == "deploy" {
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
	request.Apply = *yes
	request.Service = *service
	request.Candidate = *candidate
	var archive []byte
	if maintenance {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return lifecycleError(stderr, err)
		}
		request.Operation = hex.EncodeToString(id[:])
	}
	if action == "deploy" {
		request.Config = app
		request.Supersede = *supersede
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return lifecycleError(stderr, err)
		}
		request.Operation = hex.EncodeToString(id[:])
		request.ContextFormat = "bundle-v1"
		archive, err = buildcontext.Bundle(*filename, app, deps.Archive)
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
	if dir != "" && *serverFlag == "" {
		if err := bindProject(dir, name, server); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var binary []byte
	if action == "deploy" {
		if _, err := fmt.Fprintln(stdout, "Whole-app deploy: private services use stop/start replacement; a brief interruption is expected."); err != nil {
			return lifecycleError(stderr, err)
		}
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
	response, err := deps.Run(ctx, server.Target, request, binary, archive)
	if err != nil {
		code := lifecycleError(stderr, err)
		if mutation {
			fmt.Fprintf(stderr, "Inspect: soar status --server %s --app %s\nIf recorded, resume: soar resume --server %s --app %s %s\n", server.Name, name, server.Name, name, request.Operation)
		}
		return code
	}
	if action == "logs" {
		if response.LogService != "" || response.LogContainer != "" {
			if _, err := fmt.Fprintf(stdout, "Logs: service=%q container=%q candidate=%t\n", response.LogService, response.LogContainer, response.LogCandidate); err != nil {
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
			if ss.Candidate != "" {
				if _, err := fmt.Fprintf(stdout, "%s candidate: %q\n", serviceName, ss.Candidate); err != nil {
					return lifecycleError(stderr, err)
				}
			}
			if _, err := fmt.Fprintf(stdout, "%s / %s / %s: kind=%q phase=%q running=%t available=%t\nURL: %s\nOperation: %q\nContainer: %q\n", server.Name, name, serviceName, ss.Kind, ss.Phase, ss.Running, ss.Available, ss.URL, ss.Operation, ss.Container); err != nil {
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
