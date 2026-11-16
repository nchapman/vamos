package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/registry"
	"github.com/nchapman/vamos/internal/ssh"
)

// serverConnect recovers local access without invoking bootstrap or inspecting
// app-owned resources. A populated or interrupted app must not prevent recovery.
func serverConnect(ctx context.Context, args []string, stdout, stderr io.Writer, deps lifecycleDeps) int {
	flags := flag.NewFlagSet("server connect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	domain := flags.String("domain", "", "expected installed base domain (default: verified host domain)")
	timeout := flags.Duration("timeout", 15*time.Second, "read-only verification deadline")
	flags.Usage = func() { writeServerHelp(flags.Output(), "connect", flags) }
	if err := parseFlags(flags, args, stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 2 || *timeout <= 0 {
		return usageError(stderr, "connect requires [--domain DOMAIN] NAME TARGET and a positive timeout")
	}
	name := flags.Arg(0)
	checkDomain := *domain
	if checkDomain == "" {
		checkDomain = "validation.invalid"
	}
	if err := lifecycle.ValidateConfig(lifecycle.Config{Name: name, Domain: checkDomain}); err != nil {
		return usageError(stderr, err.Error())
	}
	target := flags.Arg(1)
	if err := ssh.ValidateTarget(target); err != nil {
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
	if previous, ok := local.Servers[name]; ok && (previous.Target != target || (*domain != "" && previous.Domain != *domain)) {
		return usageError(stderr, "server name is already registered with a different target or domain")
	}
	for other, previous := range local.Servers {
		if other != name && previous.Target == target {
			return usageError(stderr, "target is already registered under another name")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	report, err := deps.Inspect(ctx, target)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if report == nil || !report.Supported || !report.Root {
		return lifecycleError(stderr, errors.New("connect requires root on a supported Ubuntu host"))
	}
	status, err := deps.Status(ctx, target)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if status == nil || status.Active {
		return lifecycleError(stderr, errors.New("host operation state is unavailable or active; inspect server status before connecting"))
	}
	state := status.State
	if err := lifecycle.ValidateState(state); err != nil {
		return lifecycleError(stderr, errors.New("host has no valid ownership manifest; refusing registration"))
	}
	if state.Phase != "ready" || state.Config.Name != name || (*domain != "" && state.Config.Domain != *domain) {
		return lifecycleError(stderr, errors.New("host must be ready with the requested name and domain; refusing registration"))
	}
	if previous, ok := local.Servers[name]; ok && previous.Domain != state.Config.Domain {
		return lifecycleError(stderr, errors.New("server name is already registered with a different domain"))
	}
	server := registry.Server{Name: name, Target: target, Domain: state.Config.Domain}
	if err := store.Put(server); err != nil {
		return lifecycleError(stderr, fmt.Errorf("host verified but local registration failed: %w; rerun connect with the same identity", err))
	}
	if _, err := fmt.Fprintf(stdout, "Registered %s / %s (installation %s). No remote changes made.\n", server.Name, server.Target, state.ID); err != nil {
		return lifecycleError(stderr, err)
	}
	return 0
}
