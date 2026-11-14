package cli

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"slices"
)

// Standard flag parsing invokes Usage before returning ErrHelp. Capture that
// output so successful help goes to stdout, but parse failures stay on stderr.
func parseFlags(flags *flag.FlagSet, args []string, stdout, stderr io.Writer) error {
	var output bytes.Buffer
	flags.SetOutput(&output)
	err := flags.Parse(args)
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
		if len(path) >= 2 && path[1] != "repository" {
			return nil, false
		}
		if len(path) == 3 && !slices.Contains([]string{"add", "list", "show", "verify", "resume"}, path[2]) {
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
		children = []string{"inspect", "add", "status", "teardown"}
	case "service":
		children = []string{"bind", "remove"}
	case "secrets":
		children = []string{"stage", "list"}
	case "apps", "init", "exec", "console", "run", "deploy", "status", "logs", "rollback", "resume", "cleanup", "destroy", "validate", "version":
	default:
		return nil, false
	}
	if len(path) == 2 && !slices.Contains(children, path[1]) {
		return nil, false
	}
	return append(append([]string{}, path...), "--help"), true
}
