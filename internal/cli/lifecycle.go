package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/nchapman/soar/internal/executor"
	"github.com/nchapman/soar/internal/host"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/registry"
	"github.com/nchapman/soar/internal/ssh"
)

type lifecycleDeps struct {
	Registry func() (*registry.Store, error)
	Inspect  func(context.Context, string) (*host.Report, error)
	Artifact func(string, string) ([]byte, error)
	Run      func(context.Context, string, executor.Request, []byte) (*lifecycle.State, error)
	Status   func(context.Context, string) (*ssh.Status, error)
}

func defaultLifecycleDeps() lifecycleDeps {
	return lifecycleDeps{registry.Default, ssh.Inspect, ssh.Artifact, ssh.RunLifecycle, ssh.HostStatus}
}

func serverLifecycle(ctx context.Context, operation string, args []string, stdout, stderr io.Writer, deps lifecycleDeps) int {
	flags := flag.NewFlagSet("server "+operation, flag.ContinueOnError)
	flags.SetOutput(stderr)
	yes := flags.Bool("yes", false, "authorize host changes")
	purge := flags.Bool("purge", false, "authorize removal of owned runtime data and certificates")
	domain := flags.String("domain", "", "base domain (DNS is configured separately)")
	artifact := flags.String("executor", "", "Linux executor artifact override")
	asJSON := flags.Bool("json", false, "print status as JSON")
	timeoutDefault := 25 * time.Minute
	if operation == "status" {
		timeoutDefault = 15 * time.Second
	}
	timeout := flags.Duration("timeout", timeoutDefault, "client wait deadline; remote mutations survive disconnect")
	flags.Usage = func() { writeServerHelp(flags.Output(), operation, flags) }
	if err := parseFlags(flags, args, stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *timeout <= 0 || flags.NArg() < 1 || flags.NArg() > 2 {
		return usageError(stderr, "provide NAME and optional TARGET, with flags before positional arguments")
	}
	if operation != "status" && !*yes {
		return usageError(stderr, "host mutation requires --yes after reviewing the target and operation")
	}
	if operation == "teardown" && !*purge {
		return usageError(stderr, "teardown requires --purge and --yes; workloads will still block it")
	}
	if (operation != "teardown" && *purge) || (operation != "add" && *domain != "") || (operation != "status" && *asJSON) {
		return usageError(stderr, "option does not apply to this server operation")
	}
	name := flags.Arg(0)
	if err := lifecycle.ValidateConfig(lifecycle.Config{Name: name, Domain: "validation.invalid"}); err != nil {
		return usageError(stderr, err.Error())
	}
	store, err := deps.Registry()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	local, err := store.Read()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	server, registered := local.Servers[name]
	if flags.NArg() == 2 {
		target := flags.Arg(1)
		if registered && server.Target != target {
			return usageError(stderr, "TARGET differs from the registered server; refusing a target override")
		}
		server.Name = name
		server.Target = target
	}
	if server.Target == "" {
		return usageError(stderr, "server is not registered; provide NAME TARGET to recover it")
	}
	if err := ssh.ValidateTarget(server.Target); err != nil {
		return usageError(stderr, err.Error())
	}
	if operation == "add" {
		if flags.NArg() != 2 || *domain == "" {
			return usageError(stderr, "add requires --domain DOMAIN NAME TARGET")
		}
		if registered && server.Domain != *domain {
			return usageError(stderr, "server name is already registered with a different domain")
		}
		server.Domain = *domain
		if err := lifecycle.ValidateConfig(lifecycle.Config{Name: name, Domain: server.Domain}); err != nil {
			return usageError(stderr, err.Error())
		}
		for other, entry := range local.Servers {
			if other != name && entry.Target == server.Target {
				return usageError(stderr, "target is already registered under another name")
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	if operation == "status" {
		status, err := deps.Status(ctx, server.Target)
		if err != nil {
			return lifecycleError(stderr, err)
		}
		if *asJSON {
			if err := json.NewEncoder(stdout).Encode(status); err != nil {
				return lifecycleError(stderr, err)
			}
		} else if p := presentationFor(stdout); p != nil {
			if err := p.write(stdout, p.renderServerStatus(name, server.Target, status)); err != nil {
				return lifecycleError(stderr, err)
			}
		} else {
			phase := "unmanaged"
			step := ""
			if status.State != nil {
				phase = status.State.Phase
				step = status.State.Step
			}
			if _, err := fmt.Fprintf(stdout, "%s / %s: phase=%q step=%q active=%t\n", name, server.Target, phase, step, status.Active); err != nil {
				return lifecycleError(stderr, err)
			}
			if status.State != nil && status.State.LastError != "" {
				if _, err := fmt.Fprintf(stdout, "Last error: %q\n", status.State.LastError); err != nil {
					return lifecycleError(stderr, err)
				}
			}
		}
		return 0
	}
	if operation == "teardown" {
		status, err := deps.Status(ctx, server.Target)
		if err != nil {
			return lifecycleError(stderr, err)
		}
		if status.Active {
			return lifecycleError(stderr, errors.New("a host operation is active; inspect status and wait for it to finish"))
		}
		if status.State == nil || status.State.Config.Name != name {
			return lifecycleError(stderr, errors.New("host has no matching ownership manifest; refusing teardown"))
		}
		server.Domain = status.State.Config.Domain
	}
	if err := writeServerMutationStart(stdout, operation, name, server.Target, server.Domain); err != nil {
		return lifecycleError(stderr, err)
	}
	inspectCtx, cancelInspect := context.WithTimeout(ctx, 15*time.Second)
	report, err := deps.Inspect(inspectCtx, server.Target)
	cancelInspect()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if !report.Supported || !report.Root {
		return lifecycleError(stderr, errors.New("setup/teardown requires root on a supported Ubuntu host"))
	}
	binary, err := deps.Artifact(*artifact, report.Architecture)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	remoteOp := "bootstrap"
	if operation == "teardown" {
		remoteOp = "teardown"
	}
	request := executor.Request{Version: executor.Version, Operation: remoteOp, Config: &lifecycle.Config{Name: name, Domain: server.Domain}, Confirm: true, Purge: *purge}
	state, err := deps.Run(ctx, server.Target, request, binary)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if operation == "add" {
		if state.Phase != "ready" {
			return lifecycleError(stderr, errors.New("bootstrap did not return ready; inspect server status"))
		}
		if err := store.Put(server); err != nil {
			return lifecycleError(stderr, fmt.Errorf("host is ready but local registration failed: %w; rerun add with the same identity", err))
		}
	} else {
		if state.Phase != "removed" {
			return lifecycleError(stderr, errors.New("teardown did not return removed; inspect server status"))
		}
		if err := store.Remove(name); err != nil {
			return lifecycleError(stderr, fmt.Errorf("host removed but local registration cleanup failed: %w", err))
		}
	}
	message := "Owned installation removed. A small host receipt and the runtime lock are retained for safe retries."
	if operation == "add" {
		message = "Docker/Buildx and Caddy verified. No app routes or certificates configured; set DNS separately."
	}
	if err := writeServerMutationResult(stdout, name, state, message); err != nil {
		return lifecycleError(stderr, err)
	}
	return 0
}

func lifecycleError(stderr io.Writer, err error) int {
	if view := presentationFor(stderr); view != nil {
		_ = view.write(stderr, view.ink("error", "Error", true)+"\n"+view.prose(err.Error()))
		return 1
	}
	fmt.Fprintf(stderr, "error: %v\n", err)
	return 1
}
