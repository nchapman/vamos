package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/nchapman/vamos/internal/bridgeacl"
	"github.com/nchapman/vamos/internal/lifecycle"
)

const principalHelp = `Usage:
  vamos server principal grant --yes --app APP --key PUBLIC_KEY_FILE [--server SERVER] NAME
  vamos server principal list [--json] [--server SERVER]
  vamos server principal revoke --yes [--server SERVER] NAME

Manage named, single-app deploy keys using root or passwordless-sudo SSH.
SERVER defaults to the registered default server. Flags precede NAME.
--executor FILE selects the matching Linux artifact; --timeout bounds the client wait (default 1m).
List shows only these managed keys, not bootstrap or manually configured access.
To rotate, grant and test a second named key before revoking the first.
`

func serverPrincipal(ctx context.Context, args []string, stdout, stderr io.Writer, deps lifecycleDeps) int {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help")) {
		_ = writeHelpText(stdout, principalHelp)
		return 0
	}
	req := bridgeacl.Request{Action: args[0]}
	flags := flag.NewFlagSet("server principal "+req.Action, flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprint(flags.Output(), principalHelp) }
	name := flags.String("server", "", "registered server")
	artifact := flags.String("executor", "", "matching installed Linux executor artifact")
	timeout := flags.Duration("timeout", time.Minute, "client wait deadline")
	var asJSON, yes bool
	var keyPath string
	switch req.Action {
	case "list":
		flags.BoolVar(&asJSON, "json", false, "print managed grants as JSON")
	case "grant", "revoke":
		flags.BoolVar(&yes, "yes", false, "confirm key change")
		if req.Action == "grant" {
			flags.StringVar(&req.App, "app", "", "single app to authorize")
			flags.StringVar(&keyPath, "key", "", "OpenSSH public key file")
		}
	default:
		return usageError(stderr, "principal operation must be grant, list, or revoke")
	}
	if err := parseFlags(flags, args[1:], stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *timeout <= 0 {
		return usageError(stderr, "timeout must be positive")
	}
	if req.Action == "list" {
		if flags.NArg() != 0 {
			return usageError(stderr, "list takes no positional arguments; select the server with --server")
		}
	} else {
		if !yes || flags.NArg() != 1 {
			return usageError(stderr, "principal changes require --yes and one key NAME")
		}
		req.Name = flags.Arg(0)
		if req.Action == "grant" {
			if req.App == "" || keyPath == "" {
				return usageError(stderr, "grant requires --app APP and --key PUBLIC_KEY_FILE")
			}
			key, err := readDeployKey(keyPath, nil)
			if err != nil {
				return usageError(stderr, err.Error())
			}
			req.PublicKey = key
		}
	}
	if err := req.Validate(); err != nil {
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
	if *name == "" {
		*name = local.Default
	}
	server, ok := local.Servers[*name]
	if !ok {
		return usageError(stderr, "no matching registered server; provide --server or run server connect")
	}
	cfg := lifecycle.Config{Name: server.Name, Domain: server.Domain}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	binary, err := matchingAdminArtifact(ctx, deps, server.Target, cfg, *artifact)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if req.Action != "list" {
		if _, err := fmt.Fprintf(stderr, "%s / %s: %s deploy key %s\n", cfg.Name, server.Target, req.Action, req.Name); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	entries, err := deps.Principal(ctx, server.Target, cfg, req, binary)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	switch {
	case req.Action == "grant":
		_, err = fmt.Fprintf(stdout, "%s: deploy key %s granted for app %s.\n", cfg.Name, req.Name, req.App)
	case req.Action == "revoke":
		_, err = fmt.Fprintf(stdout, "%s: deploy key %s revoked.\n", cfg.Name, req.Name)
	case asJSON:
		if entries == nil {
			entries = []bridgeacl.Entry{}
		}
		err = json.NewEncoder(stdout).Encode(entries)
	case len(entries) == 0:
		_, err = fmt.Fprintln(stdout, "No managed deploy keys.")
	default:
		for _, e := range entries {
			if _, err = fmt.Fprintf(stdout, "%s\t%s\t%s\n", e.Name, e.App, e.Fingerprint); err != nil {
				break
			}
		}
	}
	if err != nil {
		return lifecycleError(stderr, err)
	}
	return 0
}
