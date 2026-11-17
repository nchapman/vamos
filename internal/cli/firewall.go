package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/firewall"
	"github.com/nchapman/vamos/internal/lifecycle"
	"golang.org/x/term"
)

const firewallHelp = `Usage:
  vamos server firewall enable --yes [--executor FILE] [NAME]
  vamos server firewall disable --yes [--executor FILE] [NAME]
  vamos server firewall status [--json] [--executor FILE] [NAME]

NAME defaults to the registered default server. Requires root SSH and a matching
installed executor artifact. --timeout bounds client waiting (default 4m).
Opt-in automatic policy for dedicated hosts: preserve discovered SSH ports,
allow HTTP/HTTPS and intentional Docker public publishing, deny other inbound
traffic. Unsupported existing firewall configurations are refused, not replaced.
Enable confirms through a fresh SSH connection; otherwise independent rollback
restores the previous rules after five minutes. Disable removes protection only;
it does not stop containers or remove their published ports. No custom rules.
`

func firewallTerminalInput(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func chooseFirewall(value string, provided bool, deps lifecycleDeps, out io.Writer) (bool, error) {
	if provided {
		switch value {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return false, errors.New("--firewall requires an explicit true or false")
		}
	}
	terminal := deps.IsTerminal
	if terminal == nil {
		terminal = firewallTerminalInput
	}
	if deps.Input == nil || !terminal(deps.Input) {
		return false, errors.New("noninteractive server add requires --firewall=true or --firewall=false")
	}
	if _, err := fmt.Fprint(out, "Enable managed firewall on this dedicated host? Allows SSH, HTTP/HTTPS and Docker public publishing; unsupported existing firewalls are refused. [y/n]: "); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(deps.Input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	}
	return false, errors.New("choose yes or no, or pass --firewall=true|false")
}

func enableAndConfirmFirewall(ctx context.Context, target string, cfg *lifecycle.Config, binary []byte, run func(context.Context, string, executor.Request, []byte) (*firewall.Status, error)) (*firewall.Status, error) {
	invoke := func(request executor.Request) (*firewall.Status, error) {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return run(callCtx, target, request, binary)
	}
	request := executor.Request{Version: executor.Version, Operation: "firewall-enable", Config: cfg, Confirm: true}
	status, err := invoke(request)
	if err != nil {
		return nil, fmt.Errorf("enable outcome may be unknown; unconfirmed changes automatically roll back after five minutes; check server firewall status: %w", err)
	}
	if status == nil || !status.Enabled || status.Degraded {
		return nil, errors.New("firewall enable did not succeed; unconfirmed changes automatically roll back after five minutes")
	}
	if !status.Pending {
		return status, nil
	}
	if status.Token == "" {
		return nil, errors.New("firewall enable returned no confirmation token; automatic rollback remains armed for five minutes")
	}
	request.Operation = "firewall-confirm"
	request.Token = status.Token
	// Deliberately a second transport invocation: never confirm inside the first
	// remote executor or reuse an SSH master connection.
	status, err = invoke(request)
	if err != nil {
		return nil, fmt.Errorf("fresh SSH confirmation failed; automatic rollback remains armed for five minutes unless confirmation reached the server; check server firewall status before retrying: %w", err)
	}
	if status == nil || !status.Enabled || status.Pending || status.Degraded {
		return nil, errors.New("firewall confirmation was not successful; automatic rollback may remain armed; check server firewall status")
	}
	return status, nil
}

func serverFirewall(ctx context.Context, args []string, stdout, stderr io.Writer, deps lifecycleDeps) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprint(stdout, firewallHelp)
		return 0
	}
	op := args[0]
	if op != "enable" && op != "disable" && op != "status" {
		return usageError(stderr, "firewall operation must be enable, disable, or status")
	}
	flags := flag.NewFlagSet("server firewall "+op, flag.ContinueOnError)
	flags.SetOutput(stderr)
	yes := flags.Bool("yes", false, "authorize firewall changes")
	asJSON := flags.Bool("json", false, "print status as JSON")
	artifact := flags.String("executor", "", "matching installed Linux executor artifact")
	timeout := flags.Duration("timeout", 4*time.Minute, "client wait deadline")
	flags.Usage = func() { fmt.Fprint(flags.Output(), firewallHelp) }
	if err := parseFlags(flags, args[1:], stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	provided := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if flags.NArg() > 1 || *timeout <= 0 {
		return usageError(stderr, "firewall accepts optional NAME and a positive timeout")
	}
	if op == "status" && provided["yes"] || op != "status" && (provided["json"] || !*yes) {
		return usageError(stderr, "firewall mutations require --yes; --json applies only to status")
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
	}
	server, ok := local.Servers[name]
	if !ok {
		return usageError(stderr, "no matching registered server; provide NAME or run server add")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	hostStatus, err := deps.Status(ctx, server.Target)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	cfg := lifecycle.Config{Name: name, Domain: server.Domain}
	if hostStatus == nil || hostStatus.Active || hostStatus.State == nil || hostStatus.State.Config != cfg || hostStatus.State.Phase != "ready" {
		return lifecycleError(stderr, errors.New("firewall requires an idle ready host with matching ownership identity"))
	}
	inspectCtx, cancelInspect := context.WithTimeout(ctx, 15*time.Second)
	report, err := deps.Inspect(inspectCtx, server.Target)
	cancelInspect()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if report == nil || !report.Supported || !report.Root {
		return lifecycleError(stderr, errors.New("managed firewall requires root SSH on a supported Ubuntu host"))
	}
	binary, err := deps.Artifact(*artifact, report.Architecture)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(binary)) != hostStatus.State.ExecutorSHA {
		return lifecycleError(stderr, errors.New("executor artifact does not match the installed host bundle; use a matching --executor or server upgrade"))
	}
	var status *firewall.Status
	if op == "enable" {
		status, err = enableAndConfirmFirewall(ctx, server.Target, &cfg, binary, deps.Firewall)
	} else {
		status, err = deps.Firewall(ctx, server.Target, executor.Request{Version: executor.Version, Operation: "firewall-" + op, Config: &cfg, Confirm: op != "status"}, binary)
	}
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if status == nil {
		return lifecycleError(stderr, errors.New("missing firewall status"))
	}
	if *asJSON {
		err = json.NewEncoder(stdout).Encode(status)
	} else {
		err = renderFirewallStatus(stdout, name, status)
	}
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if status.Degraded {
		return 1
	}
	return 0
}

func renderFirewallStatus(out io.Writer, name string, s *firewall.Status) error {
	state := "disabled"
	if s.Enabled {
		state = "enabled"
	}
	if s.Pending {
		state += " / pending confirmation (automatic rollback armed)"
	}
	if s.Degraded {
		state += " / degraded"
	}
	if _, err := fmt.Fprintf(out, "%s firewall: %s\nSSH allowances (TCP): %v\n", name, state, s.SSHPorts); err != nil {
		return err
	}
	for _, problem := range s.Problems {
		if _, err := fmt.Fprintf(out, "- %s\n", problem); err != nil {
			return err
		}
	}
	return nil
}
