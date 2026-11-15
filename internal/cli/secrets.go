package cli

import (
	"bytes"
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
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/secrets"
	"github.com/nchapman/vamos/internal/ssh"
	"golang.org/x/term"
)

const secretsHelp = `Usage:
  vamos secrets stage|set [-f vamos.yaml | --server NAME --app APP] [--stdin] [--json] NAME [NAME...]
  vamos secrets stage|set [-f vamos.yaml | --server NAME --app APP] [--json] NAME=VALUE [NAME=VALUE...]
  vamos secrets list [-f vamos.yaml | --server NAME --app APP] [--json]
All flags precede NAME. All commands accept --executor FILE and --timeout DURATION.
Stage/set prompt separately without echo. Singleton --stdin reads one bounded value;
batch --stdin reads literal NAME=value lines, exactly once per requested name.
Use either names or NAME=VALUE pairs, never a mixture; pairs cannot use --stdin.
Inline values may appear in shell history/process listings; prefer prompts or stdin.
No environment-variable lookup or value-file flags are supported.
The terminal editor accepts fewer than 4096 input bytes; --stdin accepts 8192 value bytes.
`

type secretInputFunc func(context.Context, io.Reader, io.Writer, bool) ([]byte, error)

func secretsCommand(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer, deps appDeps, read secretInputFunc) int {
	if len(args) == 0 {
		return usageError(stderr, secretsHelp)
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		_ = writeHelpText(stdout, secretsHelp)
		return 0
	}
	action := args[0]
	if action != "stage" && action != "set" && action != "list" {
		return usageError(stderr, "expected secrets stage, set or list")
	}
	f := flag.NewFlagSet("secrets "+action, flag.ContinueOnError)
	f.SetOutput(stderr)
	filename := f.String("f", config.DefaultFile, "app configuration")
	serverName := f.String("server", "", "registered server")
	appName := f.String("app", "", "app outside project (requires --server)")
	artifact := f.String("executor", "", "executor override")
	deadline := 15 * time.Second
	if action != "list" {
		deadline = 25 * time.Minute
	}
	timeout := f.Duration("timeout", deadline, "deadline")
	asJSON := f.Bool("json", false, "full metadata response")
	fromStdin := f.Bool("stdin", false, "read value from stdin")
	f.Usage = func() { fmt.Fprint(f.Output(), secretsHelp) }
	// flag's error messages can echo a malformed NAME=VALUE argument. Keep
	// help output, but never forward argument-bearing parse diagnostics.
	if err := parseFlags(f, args[1:], stdout, io.Discard); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageError(stderr, "invalid secret command flags; flags must precede secret names")
	}
	stdinSet := false
	f.Visit(func(v *flag.Flag) {
		if v.Name == "stdin" {
			stdinSet = true
		}
	})
	if *timeout <= 0 || (action != "list" && (f.NArg() < 1 || f.NArg() > secrets.MaxNames)) || (action == "list" && (f.NArg() != 0 || stdinSet)) {
		return usageError(stderr, "invalid secret command arguments")
	}
	names, inlineValues, err := secretArguments(f.Args(), stdinSet)
	if err != nil {
		return usageError(stderr, err.Error())
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
	if action != "list" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return lifecycleError(stderr, err)
		}
		r.Operation = hex.EncodeToString(id[:])
		if len(names) == 1 {
			r.SecretName = names[0]
		} else {
			r.SecretNames = names
		}
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
	if action != "list" {
		targetOut := stdout
		if *asJSON {
			targetOut = stderr
		}
		for _, n := range names {
			if err := writeSecretStageStart(targetOut, server.Name, server.Target, name, n, secretVersion(r, n)); err != nil {
				return 1
			}
		}
		fmt.Fprintf(targetOut, "Operation: %s\n", r.Operation)
		values := inlineValues
		if values != nil {
			fmt.Fprintln(stderr, "Warning: inline secret values may appear in shell history and process listings; prefer hidden prompts or --stdin.")
		} else if len(names) > 1 && *fromStdin {
			values, err = readSecretBatch(ctx, input, names)
		} else {
			values = map[string][]byte{}
			for _, n := range names {
				if !*fromStdin {
					fmt.Fprintf(stderr, "Secret %s\n", n)
				}
				var v []byte
				v, err = read(ctx, input, stderr, *fromStdin)
				if err == nil {
					err = secrets.ValidateValue(v)
				}
				if err != nil {
					break
				}
				values[n] = v
			}
		}
		if err != nil {
			return lifecycleError(stderr, errors.New("secret input failed or cancelled (values must be nonempty, bounded, single-line UTF-8)"))
		}
		if len(names) == 1 {
			value = values[r.SecretName]
		} else {
			value, err = secrets.EncodeBatch(values)
		}
		if err != nil {
			return lifecycleError(stderr, errors.New("invalid secret values"))
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
	if err != nil || response.Error != "" || response.ErrorCode != "" || response.Version != 1 || (action != "list" && !validSecretAck(r, response)) {
		if action == "set" {
			if response.Version == 1 && response.Error != "" {
				if message := deploy.RefusalMessage(r.Action, response.ErrorCode); message != "" {
					fmt.Fprintln(stderr, message)
				}
			}
			fmt.Fprintf(stderr, "Secret set did not complete; inspect staged versions and deployment status before retrying. Operation: %s\n", r.Operation)
			fmt.Fprintf(stderr, "Inspect: vamos secrets list --server %s --app %s\nInspect: vamos status --server %s --app %s\nResume with vamos resume --server %s --app %s %s only if status records this incomplete operation.\n", server.Name, name, server.Name, name, server.Name, name, r.Operation)
			return 1
		}
		if response.Version == 1 && response.Error != "" {
			if message := deploy.RefusalMessage(r.Action, response.ErrorCode); message != "" {
				writeSecretFailure(stderr, action, server.Name, server.Target, name, r.Operation, message)
				return 1
			}
		}
		if action != "list" {
			writeSecretFailure(stderr, action, server.Name, server.Target, name, r.Operation, "")
			return 1
		}
		return lifecycleError(stderr, errors.New("secret metadata request failed"))
	}
	// Only emit the metadata protocol fields, never logs or remote diagnostics.
	safe := deploy.Response{Version: response.Version, SetApplied: response.SetApplied, Secret: response.Secret, SecretVersions: response.SecretVersions, Status: response.Status}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(safe); err != nil {
			return 1
		}
		return 0
	}
	if action == "set" {
		if response.Status == nil {
			fmt.Fprintln(stdout, "Secret versions staged; no deployed consumers to update.")
		} else {
			fmt.Fprintln(stdout, "Secrets set and applied to their consumers using deployed images and configuration.")
		}
		return 0
	}
	if action == "stage" && len(r.SecretNames) > 0 {
		fmt.Fprintln(stdout, "Secret versions staged; deploy to apply.")
		return 0
	}

	if action != "list" {
		if p := presentationFor(stdout); p != nil {
			text := p.ink("success", "Secret version staged; deploy to apply.", true) + "\n"
			text += p.field("Secret", r.SecretName) + p.field("Version", r.Operation)
			if err := p.write(stdout, text); err != nil {
				return 1
			}
			return 0
		}
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
	if p := presentationFor(stdout); p != nil {
		if err := p.write(stdout, p.renderSecretList(server.Name, server.Target, name, response.SecretVersions)); err != nil {
			return 1
		}
		return 0
	}
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

// Parse all arguments before target inspection or prompting. Errors are static:
// malformed names and values must never be reflected into terminal output.
func secretArguments(args []string, stdinSet bool) ([]string, map[string][]byte, error) {
	names := make([]string, 0, len(args))
	values := map[string][]byte{}
	seen := map[string]bool{}
	for _, arg := range args {
		name, value, inline := strings.Cut(arg, "=")
		if !secrets.ValidName(name) || seen[name] {
			return nil, nil, errors.New("invalid or duplicate secret name")
		}
		seen[name] = true
		names = append(names, name)
		if inline {
			if secrets.ValidateValue([]byte(value)) != nil {
				return nil, nil, errors.New("invalid inline secret value; values must be nonempty, bounded, single-line UTF-8")
			}
			values[name] = []byte(value)
		}
	}
	if len(values) == 0 {
		return names, nil, nil
	}
	if stdinSet || len(values) != len(names) {
		return nil, nil, errors.New("use either secret names or NAME=VALUE pairs; pairs cannot be mixed with names or --stdin")
	}
	return names, values, nil
}

func secretVersion(r deploy.Request, name string) string {
	if len(r.SecretNames) == 0 {
		return r.Operation
	}
	return secrets.VersionFor(r.Operation, name)
}

func validSecretAck(r deploy.Request, response deploy.Response) bool {
	if r.Action == "secrets-set" && !response.SetApplied {
		return false
	}
	names := r.SecretNames
	entries := response.SecretVersions
	if len(names) == 0 {
		names = []string{r.SecretName}
		if response.Secret == nil || len(response.SecretVersions) != 0 {
			return false
		}
		entries = []secrets.Entry{*response.Secret}
	}
	if len(entries) != len(names) || (len(r.SecretNames) != 0 && response.Secret != nil) {
		return false
	}
	expected := map[string]string{}
	for _, n := range names {
		expected[n] = secretVersion(r, n)
	}
	for _, e := range entries {
		if expected[e.Name] == "" || expected[e.Name] != e.Version {
			return false
		}
		delete(expected, e.Name)
	}
	return len(expected) == 0
}

func readSecretBatch(ctx context.Context, input io.Reader, names []string) (map[string][]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	const bound = secrets.MaxNames * (secrets.MaxValueBytes + 66 + 2)
	go func() { b, e := io.ReadAll(io.LimitReader(input, bound+1)); ch <- result{b, e} }()
	var data []byte
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		data = r.data
	case <-ctx.Done():
		if c, ok := input.(io.Closer); ok {
			_ = c.Close()
		}
		return nil, ctx.Err()
	}
	invalid := errors.New("invalid secret batch input")
	if len(data) > bound {
		return nil, invalid
	}
	requested := map[string]bool{}
	for _, n := range names {
		requested[n] = true
	}
	values := map[string][]byte{}
	lines := bytes.Split(data, []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		// CR is framing only when immediately followed by LF.
		if i < len(lines)-1 || bytes.HasSuffix(data, []byte{'\n'}) {
			line = bytes.TrimSuffix(line, []byte{'\r'})
		}
		n, v, ok := bytes.Cut(line, []byte{'='})
		if !ok || !requested[string(n)] || values[string(n)] != nil || secrets.ValidateValue(v) != nil {
			return nil, invalid
		}
		values[string(n)] = v
	}
	if len(values) != len(names) {
		return nil, invalid
	}
	return values, nil
}
