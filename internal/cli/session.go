package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/ssh"
	"golang.org/x/term"
)

const sessionHelp = `Usage:
  soar exec [-f soar.yaml | --server NAME --app APP] [--service NAME] [-i] [-t] -- COMMAND [ARG...]
  soar console [-f soar.yaml | --server NAME --app APP] [--service NAME] [--shell /bin/sh]

Both accept --executor FILE and --timeout DURATION (default: no session deadline).
Exec preserves arguments without an implicit shell. -i/--interactive forwards stdin;
-t/--tty requires -i and real input/output terminals. Console opens /bin/sh with
stdin and a terminal; --shell selects another executable without fallback.
Without --service, select web or the sole deployed service; ambiguous apps require it.
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
	flags.Usage = func() { fmt.Fprint(stderr, sessionHelp) }
	filename := flags.String("f", config.DefaultFile, "app configuration")
	serverFlag := flags.String("server", "", "registered server override")
	appFlag := flags.String("app", "", "explicit app outside a project; requires --server")
	service := flags.String("service", "", "existing service (default: web or sole service)")
	executorFile := flags.String("executor", "", "Linux executor artifact override")
	timeout := flags.Duration("timeout", 0, "session deadline; zero means no deadline, not guaranteed remote cancellation")
	shell := flags.String("shell", "/bin/sh", "console shell executable; console only")
	var interactive, tty bool
	flags.BoolVar(&interactive, "interactive", false, "forward stdin; exec only")
	flags.BoolVar(&interactive, "i", false, "forward stdin; exec only")
	flags.BoolVar(&tty, "tty", false, "allocate terminal; requires -i; exec only")
	flags.BoolVar(&tty, "t", false, "allocate terminal; requires -i; exec only")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var shellProvided, ioProvided bool
	flags.Visit(func(f *flag.Flag) {
		shellProvided = shellProvided || f.Name == "shell"
		ioProvided = ioProvided || f.Name == "interactive" || f.Name == "i" || f.Name == "tty" || f.Name == "t"
	})
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
	request := deploy.SessionRequest{Version: 1, Action: "exec", App: name, Service: *service, Host: lifecycle.Config{Name: server.Name, Domain: server.Domain}, Command: command, Stdin: interactive, TTY: tty}
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
	if dir != "" && *serverFlag == "" {
		if err := bindProject(dir, name, server); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	inspectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	report, err := deps.Inspect(inspectCtx, server.Target)
	cancel()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if report == nil || !report.Supported || !report.Root {
		return lifecycleError(stderr, errors.New("sessions require root on a supported Ubuntu host"))
	}
	binary, err := deps.Artifact(*executorFile, report.Architecture)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	fmt.Fprintf(stderr, "%s / %s: %s (live service access; no automatic retry)\n", server.Name, name, action)
	return runSession(ctx, server.Target, request, binary, stdin, stdout, stderr, deps)
}

func runSession(ctx context.Context, target string, request deploy.SessionRequest, binary []byte, stdin io.Reader, stdout, stderr io.Writer, deps appDeps) int {
	if deps.Stream == nil {
		return lifecycleError(stderr, errors.New("session transport unavailable"))
	}
	code, err := deps.Stream(ctx, target, request, binary, stdin, stdout, stderr)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if code < 0 || code > 254 {
		return lifecycleError(stderr, errors.New("session exit outcome unknown; do not automatically retry"))
	}
	return code
}
