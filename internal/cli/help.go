package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Standard flag parsing invokes Usage before returning ErrHelp. Capture that
// output so successful help goes to stdout, but parse failures stay on stderr.
func parseFlags(flags *flag.FlagSet, args []string, stdout, stderr io.Writer) error {
	var output bytes.Buffer
	flags.SetOutput(&output)
	err := flags.Parse(args)
	if err == nil {
		err = validateSelectorFlags(flags, args[:len(args)-flags.NArg()])
		if err != nil {
			fmt.Fprintln(&output, err)
		}
	}
	flags.SetOutput(stderr)
	dest := stderr
	if errors.Is(err, flag.ErrHelp) {
		dest = stdout
		if p := presentationFor(dest); p != nil {
			if writeErr := p.write(dest, p.renderHelp(output.String())); writeErr != nil {
				return writeErr
			}
			return err
		}
	}
	if _, writeErr := io.Copy(dest, &output); writeErr != nil {
		return writeErr
	}
	return err
}

// selectorUsageError names a registered flag, never its argument. Credential
// commands can show this diagnostic while suppressing raw flag parse errors.
type selectorUsageError string

func (e selectorUsageError) Error() string { return string(e) }

// Inspect only the successfully parsed prefix. Visit reports final values but
// loses repetitions. Skip option operands (which may themselves look like
// flags); never inspect positional arguments or a forwarded command.
func validateSelectorFlags(flags *flag.FlagSet, parsed []string) error {
	seen := map[string]bool{}
	for i := 0; i < len(parsed); i++ {
		if parsed[i] == "--" {
			break
		}
		option, value, inline := strings.Cut(parsed[i], "=")
		name := strings.TrimLeft(option, "-")
		f := flags.Lookup(name)
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !inline && !(ok && b.IsBoolFlag()) {
			i++
			value = parsed[i]
		}
		switch name {
		case "server", "app", "service", "f", "replica", "run", "supersede", "key", "deploy-key", "repository-id":
			if strings.TrimSpace(value) == "" {
				return selectorUsageError(option + " must not be empty")
			}
		case "domain", "version", "endpoint", "bucket", "prefix", "region", "recovery-file":
			// Keep command-specific defaults (e.g. an empty storage prefix).
		default:
			continue
		}
		if seen[name] {
			return selectorUsageError(option + " must not be repeated")
		}
		seen[name] = true
	}
	return nil
}

func printFlagDefaults(w io.Writer, parsed *flag.FlagSet, names ...string) {
	visible := flag.NewFlagSet(parsed.Name(), flag.ContinueOnError)
	visible.SetOutput(w)
	parsed.VisitAll(func(f *flag.Flag) {
		if slices.Contains(names, f.Name) {
			visible.Var(f.Value, f.Name, f.Usage)
			visible.Lookup(f.Name).DefValue = f.DefValue
		}
	})
	visible.PrintDefaults()
}

// Only command names are accepted here: forwarding arbitrary flags/positionals
// could consume --help as a value or accidentally invoke an operation.
func commandHelpArgs(path []string) ([]string, bool) {
	if len(path) == 0 || len(path) > 3 {
		return nil, false
	}
	if path[0] == "backup" {
		if len(path) == 2 && slices.Contains([]string{"configure", "plan"}, path[1]) {
			return append(append([]string{}, path...), "--help"), true
		}
		if len(path) >= 2 && path[1] != "repository" {
			return nil, false
		}
		if len(path) == 3 && !slices.Contains([]string{"add", "list", "show", "verify", "resume"}, path[2]) {
			return nil, false
		}
		return append(append([]string{}, path...), "--help"), true
	}
	if len(path) >= 2 && path[0] == "server" && path[1] == "principal" {
		if len(path) == 3 && !slices.Contains([]string{"grant", "list", "revoke"}, path[2]) {
			return nil, false
		}
		return append(append([]string{}, path...), "--help"), true
	}
	if len(path) >= 2 && path[0] == "server" && path[1] == "firewall" {
		if len(path) == 3 && !slices.Contains([]string{"enable", "disable", "status"}, path[2]) {
			return nil, false
		}
		return append(append([]string{}, path...), "--help"), true
	}
	if len(path) > 2 {
		return nil, false
	}
	var children []string
	switch path[0] {
	case "server":
		children = []string{"inspect", "add", "upgrade", "connect", "status", "teardown", "firewall"}
	case "jobs":
		children = []string{"show"}
	case "service":
		children = []string{"bind", "remove"}
	case "secrets":
		children = []string{"stage", "set", "list"}
	case "apps", "init", "exec", "console", "run", "deploy", "status", "logs", "rollback", "resume", "cleanup", "destroy", "validate", "upgrade", "version":
	default:
		return nil, false
	}
	if len(path) == 2 && !slices.Contains(children, path[1]) {
		return nil, false
	}
	return append(append([]string{}, path...), "--help"), true
}
