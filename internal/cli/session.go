package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/ssh"
	"golang.org/x/term"
)

const sessionHelp = `Usage:
  vamos exec [-f vamos.yaml | --server NAME --app APP] [--service NAME] [--replica ORDINAL] [-i] [-t] -- COMMAND [ARG...]
  vamos console [-f vamos.yaml | --server NAME --app APP] [--service NAME] [--replica ORDINAL] [--shell /bin/sh]

Both accept --executor FILE and --timeout DURATION (default: no session deadline).
Exec preserves arguments without an implicit shell. -i/--interactive forwards stdin;
-t/--tty requires -i and real input/output terminals. Console opens /bin/sh with
stdin and a terminal; --shell selects another executable without fallback.
Without --service, select web or the sole deployed service; ambiguous apps require it.
Multi-instance services require --replica (1-based ordinal).
Commands use the current container's user, working directory, secrets and volumes.
No build, restart, secret update or automatic retry occurs. Deployment can stop the
pinned container and end the session. A disconnected command may keep running;
there is no exec resume/status record. Runtime output is not secret-redacted.
`

func terminalStreams(stdin io.Reader, stdout io.Writer) bool {
	in, inOK := stdin.(*os.File)
	out, outOK := stdout.(*os.File)
	return inOK && outOK && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

func sessionCommand(ctx context.Context, action string, args []string, stdin io.Reader, stdout, stderr io.Writer, deps appDeps, terminal func(io.Reader, io.Writer) bool) int {
	flags := flag.NewFlagSet(action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(flags.Output(), sessionHelp) }
	filename := flags.String("f", config.DefaultFile, "app configuration")
	serverFlag := flags.String("server", "", "registered server override")
	appFlag := flags.String("app", "", "explicit app outside a project; requires --server")
	service := flags.String("service", "", "existing service (default: web or sole service)")
	replica := flags.Int("replica", 0, "instance ordinal; required for multi-instance services")
	executorFile := flags.String("executor", "", "Linux executor artifact override")
	timeout := flags.Duration("timeout", 0, "session deadline; zero means no deadline, not guaranteed remote cancellation")
	shell := flags.String("shell", "/bin/sh", "console shell executable; console only")
	var interactive, tty bool
	flags.BoolVar(&interactive, "interactive", false, "forward stdin; exec only")
	flags.BoolVar(&interactive, "i", false, "forward stdin; exec only")
	flags.BoolVar(&tty, "tty", false, "allocate terminal; requires -i; exec only")
	flags.BoolVar(&tty, "t", false, "allocate terminal; requires -i; exec only")
	if err := parseFlags(flags, args, stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var shellProvided, ioProvided, replicaProvided bool
	flags.Visit(func(f *flag.Flag) {
		replicaProvided = replicaProvided || f.Name == "replica"
		shellProvided = shellProvided || f.Name == "shell"
		ioProvided = ioProvided || f.Name == "interactive" || f.Name == "i" || f.Name == "tty" || f.Name == "t"
	})
	if replicaProvided && (*replica < 1 || *replica > config.MaxReplicas) {
		return usageError(stderr, "--replica must be between 1 and 8")
	}
	if *timeout < 0 || (*appFlag != "" && *serverFlag == "") {
		return usageError(stderr, "invalid timeout or --app without --server")
	}
	command := flags.Args()
	if action == "console" {
		if len(command) != 0 || ioProvided {
			return usageError(stderr, "console accepts no command or exec I/O flags; use --shell for a different executable")
		}
		command, interactive, tty = []string{*shell}, true, true
	} else if action != "exec" || shellProvided || len(command) == 0 {
		return usageError(stderr, "exec requires a command after --; --shell is console only")
	}
	if tty && (!interactive || !terminal(stdin, stdout)) {
		return usageError(stderr, "TTY mode requires interactive stdin and real input/output terminals; use exec -i without -t for pipes")
	}
	name, dir := *appFlag, ""
	if name == "" {
		app, err := config.Load(*filename)
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
	store, err := deps.Registry()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	state, err := store.Read()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	server, err := selectAppServer(state, dir, name, *serverFlag)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	request := deploy.SessionRequest{Version: 1, Action: "exec", App: name, Service: *service, Replica: *replica, Host: lifecycle.Config{Name: server.Name, Domain: server.Domain}, Command: command, Stdin: interactive, TTY: tty}
	if dir != "" {
		bindings, err := readServiceBindings(dir, server.Target, name)
		if err != nil {
			return lifecycleError(stderr, err)
		}
		request.Bindings = make(map[string]deploy.SessionBinding)
		for serviceName, binding := range bindings {
			if *service == "" || serviceName == *service {
				request.Bindings[serviceName] = deploy.SessionBinding{HostIdentity: binding.HostIdentity, Incarnation: binding.Incarnation}
			}
		}
	}
	if err := deploy.ValidateSessionRequest(request); err != nil {
		return usageError(stderr, err.Error())
	}
	if err := ssh.ValidateTarget(server.Target); err != nil {
		return usageError(stderr, err.Error())
	}
	if dir != "" {
		if err := bindProjectForCommand(dir, name, server, *serverFlag != ""); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	fmt.Fprintf(stderr, "%s / %s: %s (live service access; no automatic retry)\n", server.Name, name, action)
	code, err := runSessionTransport(ctx, server.Target, *executorFile, request, stdin, stdout, stderr, deps)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if code < 0 || code > 254 {
		return lifecycleError(stderr, errors.New("session exit outcome unknown; do not automatically retry"))
	}
	return code
}
