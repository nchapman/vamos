package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
	"github.com/nchapman/vamos/internal/registry"
	"github.com/nchapman/vamos/internal/ssh"
)

type lifecycleDeps struct {
	Registry      func() (*registry.Store, error)
	Inspect       func(context.Context, string) (*host.Report, error)
	Artifact      func(string, string) ([]byte, error)
	Run           func(context.Context, string, executor.Request, []byte) (*lifecycle.State, error)
	Status        func(context.Context, string) (*ssh.Status, error)
	DeployKey     func() (string, error)
	ResolveDomain func(context.Context, string) (string, error)
}

func defaultLifecycleDeps() lifecycleDeps {
	return lifecycleDeps{Registry: registry.Default, Inspect: ssh.Inspect, Artifact: ssh.Artifact, Run: ssh.RunLifecycle, Status: ssh.HostStatus, DeployKey: discoverDeployKey, ResolveDomain: defaultServerDomain}
}

func serverLifecycle(ctx context.Context, operation string, args []string, stdout, stderr io.Writer, deps lifecycleDeps) int {
	if operation == "connect" {
		return serverConnect(ctx, args, stdout, stderr, deps)
	}
	flags := flag.NewFlagSet("server "+operation, flag.ContinueOnError)
	flags.SetOutput(stderr)
	yes := flags.Bool("yes", false, "authorize host changes")
	purge := flags.Bool("purge", false, "authorize removal of owned runtime data and certificates")
	domain := flags.String("domain", "", "base domain (default: <hex-ip>.tryvamos.dev)")
	artifact := flags.String("executor", "", "Linux executor artifact override")
	deployKey := flags.String("deploy-key", "", "public key file installed for the restricted deploy bridge (default: SSH agent or ~/.ssh key)")
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
	allowDefaultServer := operation == "status" || operation == "upgrade"
	if *timeout <= 0 || (!allowDefaultServer && flags.NArg() < 1) || flags.NArg() > 2 {
		return usageError(stderr, "provide NAME and optional TARGET, with flags before positional arguments")
	}
	if operation != "status" && !*yes {
		return usageError(stderr, "host mutation requires --yes after reviewing the target and operation")
	}
	if operation == "teardown" && !*purge {
		return usageError(stderr, "teardown requires --purge and --yes; workloads will still block it")
	}
	if (operation != "teardown" && *purge) || (operation != "add" && *domain != "") || (operation != "status" && *asJSON) || (operation != "add" && *deployKey != "") {
		return usageError(stderr, "option does not apply to this server operation")
	}
	store, err := deps.Registry()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	local, err := store.Read()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	name := flags.Arg(0)
	if name == "" {
		name = local.Default
		if name == "" {
			return usageError(stderr, "no default registered server; run vamos server add first or provide NAME")
		}
	}
	if err := lifecycle.ValidateConfig(lifecycle.Config{Name: name, Domain: "validation.invalid"}); err != nil {
		return usageError(stderr, err.Error())
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
		if flags.NArg() != 2 {
			return usageError(stderr, "add requires NAME TARGET")
		}
		resolvedDomain := *domain
		if resolvedDomain == "" {
			derived, deriveErr := deriveServerDomain(ctx, server.Target, deps.ResolveDomain)
			if deriveErr != nil {
				return usageError(stderr, deriveErr.Error())
			}
			resolvedDomain = derived
		}
		if registered && server.Domain != resolvedDomain {
			return usageError(stderr, "server name is already registered with a different domain")
		}
		server.Domain = resolvedDomain
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
	if operation == "teardown" || operation == "upgrade" {
		status, err := deps.Status(ctx, server.Target)
		if err != nil {
			return lifecycleError(stderr, err)
		}
		if status.Active {
			return lifecycleError(stderr, errors.New("a host operation is active; inspect status and wait for it to finish"))
		}
		if status.State == nil || status.State.Config.Name != name {
			return lifecycleError(stderr, fmt.Errorf("host has no matching ownership manifest; refusing %s", operation))
		}
		if operation == "upgrade" && status.State.Phase != "ready" {
			return lifecycleError(stderr, errors.New("host must be ready before coordinator upgrade"))
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
		return lifecycleError(stderr, errors.New("server lifecycle changes require root on a supported Ubuntu host"))
	}
	binary, err := deps.Artifact(*artifact, report.Architecture)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	remoteOp := "bootstrap"
	switch operation {
	case "teardown":
		remoteOp = "teardown"
	case "upgrade":
		remoteOp = "upgrade"
	}
	request := executor.Request{Version: executor.Version, Operation: remoteOp, Config: &lifecycle.Config{Name: name, Domain: server.Domain}, Confirm: true, Purge: *purge}
	if operation == "add" {
		key, keyErr := readDeployKey(*deployKey, deps.DeployKey)
		if keyErr != nil {
			return usageError(stderr, keyErr.Error())
		}
		request.DeployKey = key
	}
	state, err := deps.Run(ctx, server.Target, request, binary)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	switch operation {
	case "add":
		if state.Phase != "ready" {
			return lifecycleError(stderr, errors.New("bootstrap did not return ready; inspect server status"))
		}
		if err := store.Put(server); err != nil {
			return lifecycleError(stderr, fmt.Errorf("host is ready but local registration failed: %w; rerun server connect with the same identity", err))
		}
	case "teardown":
		if state.Phase != "removed" {
			return lifecycleError(stderr, errors.New("teardown did not return removed; inspect server status"))
		}
		if err := store.Remove(name); err != nil {
			return lifecycleError(stderr, fmt.Errorf("host removed but local registration cleanup failed: %w", err))
		}
	}
	message := "Installed coordinator and executor were replaced after ownership and health checks."
	switch operation {
	case "teardown":
		message = "Owned installation removed. A small host receipt and the runtime lock are retained for safe retries."
	case "add":
		message = "Docker/Buildx and Caddy verified. No app routes or certificates configured; set DNS separately."
		if strings.HasSuffix(server.Domain, ".tryvamos.dev") {
			message = fmt.Sprintf("Docker/Buildx and Caddy verified. Apps will be served under *.%s with automatic HTTPS.", server.Domain)
		}
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

// readDeployKey resolves the bootstrap bridge key: an explicit file, the SSH
// agent's first key, or the first conventional local public key file.
func readDeployKey(path string, discover func() (string, error)) (string, error) {
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("read deploy key: %w", err)
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, 4097))
		if err != nil {
			return "", fmt.Errorf("read deploy key: %w", err)
		}
		if len(data) > 4096 {
			return "", errors.New("deploy key file is too large")
		}
		line := strings.TrimSpace(string(data))
		if err := lifecycle.ValidateDeployKey(line); err != nil {
			return "", err
		}
		return line, nil
	}
	if discover == nil {
		return "", errors.New("no deploy key configured")
	}
	line, err := discover()
	if err != nil {
		return "", err
	}
	if err := lifecycle.ValidateDeployKey(line); err != nil {
		return "", fmt.Errorf("discovered deploy key is invalid: %w", err)
	}
	return line, nil
}

func discoverDeployKey() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result, err := process.Run(ctx, "ssh-add", []string{"-L"}, nil); err == nil && len(result.Stdout) != 0 {
		for _, raw := range strings.Split(string(result.Stdout), "\n") {
			line := strings.TrimSpace(raw)
			if line != "" && lifecycle.ValidateDeployKey(line) == nil {
				return line, nil
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("no SSH public key found for the restricted deploy bridge; pass --deploy-key PATH")
	}
	for _, base := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		keyPath := filepath.Join(home, ".ssh", base)
		if data, err := os.ReadFile(keyPath + ".pub"); err == nil {
			first := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
			if first != "" && lifecycle.ValidateDeployKey(first) == nil {
				return first, nil
			}
		}
		if info, err := os.Stat(keyPath); err == nil && info.Mode().IsRegular() {
			if result, err := process.Run(ctx, "ssh-keygen", []string{"-y", "-P", "", "-f", keyPath}, nil); err == nil && len(result.Stdout) != 0 {
				first := strings.TrimSpace(strings.SplitN(string(result.Stdout), "\n", 2)[0])
				if first != "" && lifecycle.ValidateDeployKey(first) == nil {
					return first, nil
				}
			}
		}
	}
	return "", errors.New("no SSH public key found for the restricted deploy bridge; pass --deploy-key PATH")
}

func targetHost(target string) string {
	if at := strings.LastIndex(target, "@"); at >= 0 {
		return target[at+1:]
	}
	return target
}

func isPublicIPv4(addr netip.Addr) bool {
	if !addr.IsValid() || !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	b := addr.As4()
	// Exclude RFC 6598 shared/CGNAT space (100.64.0.0/10, e.g. Tailscale).
	if b[0] == 100 && b[1]&0xc0 == 64 {
		return false
	}
	return true
}

func formatTryVamosDomain(addr netip.Addr) string {
	b := addr.As4()
	return fmt.Sprintf("%02x%02x%02x%02x.tryvamos.dev", b[0], b[1], b[2], b[3])
}

func deriveServerDomain(ctx context.Context, target string, resolve func(context.Context, string) (string, error)) (string, error) {
	host := targetHost(target)
	if addr, err := netip.ParseAddr(host); err == nil {
		if !isPublicIPv4(addr) {
			return "", errors.New("cannot derive a default tryvamos.dev domain from a non-public or IPv6 target; pass --domain DOMAIN")
		}
		return formatTryVamosDomain(addr), nil
	}
	if resolve == nil {
		return "", errors.New("add requires --domain DOMAIN when TARGET is not a public IPv4 address")
	}
	return resolve(ctx, target)
}

func defaultServerDomain(ctx context.Context, target string) (string, error) {
	host := targetHost(target)
	sshCtx, cancelSSH := context.WithTimeout(ctx, 2*time.Second)
	if result, err := process.Run(sshCtx, "ssh", []string{"-G", "--", target}, nil); err == nil && len(result.Stdout) != 0 {
		for _, line := range strings.Split(string(result.Stdout), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "hostname "); ok && strings.TrimSpace(rest) != "" {
				host = strings.TrimSpace(rest)
				break
			}
		}
	}
	cancelSSH()
	if addr, err := netip.ParseAddr(host); err == nil {
		if !isPublicIPv4(addr) {
			return "", errors.New("cannot derive a default tryvamos.dev domain from a non-public or IPv6 target; pass --domain DOMAIN")
		}
		return formatTryVamosDomain(addr), nil
	}
	lookupCtx, cancelLookup := context.WithTimeout(ctx, 5*time.Second)
	defer cancelLookup()
	addrs, err := net.DefaultResolver.LookupNetIP(lookupCtx, "ip4", host)
	if err != nil {
		return "", fmt.Errorf("cannot derive a default tryvamos.dev domain from TARGET (%w); pass --domain DOMAIN", err)
	}
	for _, addr := range addrs {
		if isPublicIPv4(addr) {
			return formatTryVamosDomain(addr), nil
		}
	}
	return "", errors.New("cannot derive a default tryvamos.dev domain from TARGET; pass --domain DOMAIN")
}
