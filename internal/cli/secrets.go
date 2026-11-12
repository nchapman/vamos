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

	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/secrets"
	"github.com/nchapman/soar/internal/ssh"
	"golang.org/x/term"
)

const secretsHelp = `Usage:
  soar secrets stage [-f soar.yaml | --server NAME --app APP] [--stdin] [--json] NAME
  soar secrets list [-f soar.yaml | --server NAME --app APP] [--json]
All flags precede NAME. Both commands accept --executor FILE and --timeout DURATION.
Stage prompts without echo by default; --stdin reads one bounded value.
No value arguments, environment variables, or value files are accepted.
The terminal editor accepts fewer than 4096 input bytes; --stdin accepts 8192 value bytes.
`

type secretInputFunc func(context.Context, io.Reader, io.Writer, bool) ([]byte, error)

func secretsCommand(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer, deps appDeps, read secretInputFunc) int {
	if len(args) == 0 {
		return usageError(stderr, secretsHelp)
	}
	if args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, secretsHelp)
		return 0
	}
	action := args[0]
	if action != "stage" && action != "list" {
		return usageError(stderr, "expected secrets stage or list")
	}
	f := flag.NewFlagSet("secrets "+action, flag.ContinueOnError)
	f.SetOutput(stderr)
	filename := f.String("f", config.DefaultFile, "app configuration")
	serverName := f.String("server", "", "registered server")
	appName := f.String("app", "", "app outside project (requires --server)")
	artifact := f.String("executor", "", "executor override")
	deadline := 15 * time.Second
	if action == "stage" {
		deadline = 25 * time.Minute
	}
	timeout := f.Duration("timeout", deadline, "deadline")
	asJSON := f.Bool("json", false, "full metadata response")
	fromStdin := f.Bool("stdin", false, "read value from stdin")
	f.Usage = func() { fmt.Fprint(stderr, secretsHelp) }
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	stdinSet := false
	f.Visit(func(v *flag.Flag) {
		if v.Name == "stdin" {
			stdinSet = true
		}
	})
	if *timeout <= 0 || (action == "stage" && f.NArg() != 1) || (action == "list" && (f.NArg() != 0 || stdinSet)) {
		return usageError(stderr, "invalid secret command arguments")
	}
	if action == "stage" && !secrets.ValidName(f.Arg(0)) {
		return usageError(stderr, "invalid secret name")
	}
	if *appName != "" && *serverName == "" {
		return usageError(stderr, "--app requires --server")
	}
	name := *appName
	dir := ""
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
	if err := lifecycle.ValidateConfig(lifecycle.Config{Name: name, Domain: "validation.invalid"}); err != nil {
		return usageError(stderr, "invalid app name")
	}
	store, err := deps.Registry()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	local, err := store.Read()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	server, err := selectAppServer(local, dir, name, *serverName)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if err := ssh.ValidateTarget(server.Target); err != nil {
		return lifecycleError(stderr, err)
	}
	r := deploy.Request{Version: 1, Action: "secrets-" + action, App: name, Host: lifecycle.Config{Name: server.Name, Domain: server.Domain}}
	if action == "stage" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return lifecycleError(stderr, err)
		}
		r.Operation = hex.EncodeToString(id[:])
		r.SecretName = f.Arg(0)
	}
	if err := deploy.ValidateRequest(r); err != nil {
		return lifecycleError(stderr, err)
	}
	if dir != "" && *serverName == "" {
		if err := bindProject(dir, name, server); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var value []byte
	if action == "stage" {
		targetOut := stdout
		if *asJSON {
			targetOut = stderr
		}
		if _, err := fmt.Fprintf(targetOut, "%s / %s: stage %s (operation/version %s)\n", server.Name, name, r.SecretName, r.Operation); err != nil {
			return 1
		}
		value, err = read(ctx, input, stderr, *fromStdin)
		if err != nil {
			return lifecycleError(stderr, errors.New("secret input failed or cancelled (use a terminal or --stdin; value must be nonempty, bounded, single-line UTF-8)"))
		}
		if err := secrets.ValidateValue(value); err != nil {
			return lifecycleError(stderr, errors.New("invalid secret value"))
		}
	}
	inspectCtx, stopInspect := context.WithTimeout(ctx, 15*time.Second)
	report, err := deps.Inspect(inspectCtx, server.Target)
	stopInspect()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if !report.Supported || !report.Root {
		return lifecycleError(stderr, errors.New("requires root on a supported Ubuntu host"))
	}
	binary, err := deps.Artifact(*artifact, report.Architecture)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	response, err := deps.Run(ctx, server.Target, r, binary, value)
	if err != nil || response.Error != "" {
		if action == "stage" {
			fmt.Fprintf(stderr, "Secret staging outcome unknown (operation/version %s).\nInspect: soar secrets list --server %s --app %s\n", r.Operation, server.Name, name)
			return 1
		}
		return lifecycleError(stderr, errors.New("secret metadata request failed"))
	}
	// Only emit the metadata protocol fields, never logs or remote diagnostics.
	safe := deploy.Response{Version: response.Version, Secret: response.Secret, SecretVersions: response.SecretVersions, Status: response.Status}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(safe); err != nil {
			return 1
		}
		return 0
	}
	if action == "stage" {
		fmt.Fprintf(stdout, "Staged %s version %s; deploy to apply.\n", r.SecretName, r.Operation)
		return 0
	}
	sort.Slice(response.SecretVersions, func(i, j int) bool {
		a, b := response.SecretVersions[i], response.SecretVersions[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Version < b.Version
	})
	for _, e := range response.SecretVersions {
		fmt.Fprintf(stdout, "%s %s latest=%t\n", e.Name, e.Version, e.Latest)
	}
	return 0
}

// The terminal library limits lines to 4096 runes and silently ignores excess.
// Bound raw input below that limit so it can never return a truncated value.
type passwordReader struct {
	r io.Reader
	n int
}

func (r *passwordReader) Read(p []byte) (int, error) {
	if r.n >= 4096 {
		return 0, errors.New("terminal input limit exceeded; use --stdin")
	}
	if len(p) > 4096-r.n {
		p = p[:4096-r.n]
	}
	n, e := r.r.Read(p)
	// Treat Ctrl-D as cancellation even after text (the editor otherwise only
	// treats it as EOF on an empty line). Neither control byte is a value.
	for i := 0; i < n; i++ {
		if p[i] == 4 {
			p[i] = 3
		}
	}
	r.n += n
	return n, e
}
func readSecretInput(ctx context.Context, input io.Reader, output io.Writer, fromStdin bool) (value []byte, err error) {
	var restore func() error
	var terminal *term.Terminal
	if !fromStdin {
		f, ok := input.(*os.File)
		if !ok || !term.IsTerminal(int(f.Fd())) {
			return nil, errors.New("terminal required; use --stdin")
		}
		state, e := term.MakeRaw(int(f.Fd()))
		if e != nil {
			return nil, errors.New("cannot secure terminal")
		}
		restore = func() error { return term.Restore(int(f.Fd()), state) }
		defer func() {
			if restore != nil && restore() != nil {
				value = nil
				err = errors.New("cannot restore terminal")
			}
		}()
		terminal = term.NewTerminal(struct {
			io.Reader
			io.Writer
		}{&passwordReader{r: input}, output}, "")
	}
	type result struct {
		v []byte
		e error
	}
	ch := make(chan result, 1)
	go func() {
		if fromStdin {
			v, e := secrets.ReadInput(input)
			ch <- result{v, e}
		} else {
			v, e := terminal.ReadPassword("Value: ")
			ch <- result{[]byte(v), e}
		}
	}()
	select {
	case r := <-ch:
		return r.v, r.e
	case <-ctx.Done():
		if restore != nil {
			if restore() != nil {
				return nil, errors.New("cannot restore terminal")
			}
			restore = nil
		}
		if c, ok := input.(io.Closer); ok {
			_ = c.Close()
		}
		return nil, errors.New("secret input cancelled")
	}
}
