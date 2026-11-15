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
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/backup"
	"github.com/nchapman/vamos/internal/backupbackend"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/ssh"
	"golang.org/x/term"
)

const backupHelp = `Usage:
  vamos backup configure --app APP [-f backup.yaml] [--server NAME] [--yes]
  vamos backup plan --app APP [--server NAME] [--json]
  vamos backup repository add [flags] NAME
  vamos backup repository list [--server NAME] [--json]
  vamos backup repository show [flags] NAME
  vamos backup repository verify [flags] NAME
  vamos backup repository resume [flags] NAME OPERATION
Flags precede positional arguments. All commands accept --server, --executor,
--timeout and --json. Add requires --recovery-file FILE and confirmation (--yes).
Add options: --mode new|connect --endpoint URL --bucket NAME --prefix PREFIX
--region REGION (default us-east-1) --lookup path|dns --repository-id FULL_ID
--credentials-stdin.
Credentials stdin is one bounded JSON object: access_key_id, secret_access_key,
session_token, password (connect only), ca_certificate. No secret value flags.
New repositories use an operation-scoped prefix (base default: vamos).
Connect uses the exact prefix and requires the full repository ID noninteractively.
List/show report recorded metadata; verify is read-only remote verification.
Resume uses the ORIGINAL operation ID, stored credentials and configuration.
Recovery kits contain sensitive credentials: keep an independent offline copy.
On Windows, secure the recovery directory with your own ACLs.
No application backups or schedules are created. Capture/restore are not implemented.
`

func backupRandom() (string, error) {
	var b [16]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func backupPassword() (string, error) {
	var b [32]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func backupCommand(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer, deps appDeps, secret secretInputFunc) int {
	terminal := false
	if f, ok := input.(*os.File); ok {
		terminal = term.IsTerminal(int(f.Fd()))
	}
	return backupCommandInteractive(ctx, args, input, stdout, stderr, deps, secret, terminal)
}
func backupCommandInteractive(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer, deps appDeps, secret secretInputFunc, terminal bool) int {
	if len(args) > 0 && (args[0] == "configure" || args[0] == "plan") {
		return backupPlanCommand(ctx, args, input, stdout, stderr, deps)
	}
	help := func() int { _ = writeHelpText(stdout, backupHelp); return 0 }
	if len(args) == 0 {
		return help()
	}
	if args[0] == "--help" || args[0] == "-h" {
		return help()
	}
	if args[0] != "repository" {
		return usageError(stderr, "backup capture, restore and scheduling are not implemented; expected repository")
	}
	if len(args) == 1 {
		return help()
	}
	action := args[1]
	if action == "--help" || action == "-h" {
		return help()
	}
	if action != "add" && action != "list" && action != "show" && action != "verify" && action != "resume" {
		return usageError(stderr, "unsupported backup repository command")
	}
	f := flag.NewFlagSet("backup repository "+action, flag.ContinueOnError)
	serverName := f.String("server", "", "registered server")
	executor := f.String("executor", "", "executor artifact")
	timeout := f.Duration("timeout", 5*time.Minute, "deadline")
	asJSON := f.Bool("json", false, "metadata JSON")
	var mode, endpoint, bucket, prefix, region, lookup, id, path string
	var yes, fromStdin bool
	if action == "add" {
		f.StringVar(&mode, "mode", "", "new or connect")
		f.StringVar(&endpoint, "endpoint", "", "S3 endpoint")
		f.StringVar(&bucket, "bucket", "", "bucket")
		f.StringVar(&prefix, "prefix", "", "prefix")
		f.StringVar(&region, "region", "", "region")
		f.StringVar(&lookup, "lookup", "path", "path or dns")
		f.StringVar(&id, "repository-id", "", "full expected ID")
		f.StringVar(&path, "recovery-file", "", "private client recovery kit")
		f.BoolVar(&yes, "yes", false, "authorize setup")
		f.BoolVar(&fromStdin, "credentials-stdin", false, "private JSON")
	}
	f.Usage = func() { fmt.Fprint(f.Output(), backupHelp) }
	if e := parseFlags(f, args[2:], stdout, stderr); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	count := 1
	if action == "list" {
		count = 0
	}
	if action == "resume" {
		count = 2
	}
	if f.NArg() != count || *timeout <= 0 {
		return usageError(stderr, "invalid repository arguments")
	}
	name := f.Arg(0)
	if action != "list" && !backup.ValidName(name) {
		return usageError(stderr, "invalid repository name")
	}
	store, e := deps.Registry()
	if e != nil {
		return lifecycleError(stderr, e)
	}
	state, e := store.Read()
	if e != nil {
		return lifecycleError(stderr, e)
	}
	server, e := selectAppServer(state, "", "", *serverName)
	if e != nil {
		return lifecycleError(stderr, e)
	}
	if e = ssh.ValidateTarget(server.Target); e != nil {
		return lifecycleError(stderr, e)
	}
	if e = lifecycle.ValidateConfig(lifecycle.Config{Name: server.Name, Domain: server.Domain}); e != nil {
		return lifecycleError(stderr, e)
	}
	req := backup.Request{Action: action, Name: name}
	if action == "show" {
		req.Action = "status"
	}
	if action == "resume" {
		req.Operation = f.Arg(1)
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	run := func(r backup.Request, private []byte) (*backup.Response, error) {
		response, e := runAppTransport(ctx, server.Target, *executor, deploy.Request{Version: 1, Action: "backup-repository", Host: lifecycle.Config{Name: server.Name, Domain: server.Domain}, Backup: &r}, private, deps)
		if response.Version == 1 && response.ErrorCode != "" {
			if message := deploy.RefusalMessage("backup-repository", response.ErrorCode); message != "" {
				fmt.Fprintln(stderr, message)
			}
			return nil, errors.New("repository refused")
		}
		if e != nil || response.Version != 1 || response.Error != "" || response.Backup == nil {
			return nil, errors.New("repository response unavailable or invalid")
		}
		if !backupSafeResponse(response.Backup) || !backupResponseMatches(r, response.Backup) {
			return nil, errors.New("invalid repository metadata")
		}
		return response.Backup, nil
	}
	var private []byte
	var kit backup.RecoveryKit
	var staged *backupKitFile
	if action == "add" {
		ask := func(label string, dest *string) error {
			if *dest != "" {
				return nil
			}
			fmt.Fprintf(stderr, "%s: ", label)
			line, e := backupPromptLine(input)
			if len(line) > 4096 {
				return errors.New("answer too long")
			}
			if e != nil {
				return errors.New("prompt input unavailable")
			}
			*dest = strings.TrimSpace(line)
			return nil
		}
		if terminal && !fromStdin {
			for _, p := range []struct {
				label string
				value *string
			}{{"Mode (new/connect)", &mode}, {"Endpoint", &endpoint}, {"Bucket", &bucket}, {"Prefix (new default vamos)", &prefix}, {"Region (default us-east-1)", &region}, {"Recovery file", &path}} {
				if e = ask(p.label, p.value); e != nil {
					return lifecycleError(stderr, e)
				}
			}
		}
		if (!terminal || fromStdin) && !yes {
			fmt.Fprintf(stderr, "Preview only: target %s (%s), repository %s; no credentials read or remote calls. Re-run with --yes and complete setup flags. No application backups exist.\n", server.Name, server.Target, name)
			return 0
		}
		if mode != "new" && mode != "connect" {
			return usageError(stderr, "--mode must be new or connect")
		}
		if path == "" {
			return usageError(stderr, "--recovery-file is required")
		}
		operation, e := backupRandom()
		if e != nil {
			return lifecycleError(stderr, e)
		}
		req.Operation = operation
		req.Action = "create"
		req.Confirm = true
		if mode == "new" {
			if id != "" {
				return usageError(stderr, "new repositories do not accept --repository-id")
			}
			if prefix == "" {
				prefix = "vamos"
			}
			prefix = strings.TrimRight(prefix, "/") + "/" + operation
		} else {
			req.Action = "connect"
			if (!terminal || fromStdin) && id == "" {
				return usageError(stderr, "connect requires --repository-id")
			}
		}
		if region == "" {
			region = "us-east-1"
		}
		cfg := backupbackend.Config{Endpoint: endpoint, Bucket: bucket, Prefix: prefix, Region: region, Lookup: lookup}
		req.Config = &cfg
		req.ExpectedID = id
		if e = backupbackend.ValidateConfig(cfg); e != nil {
			return usageError(stderr, "invalid repository configuration")
		}
		if id != "" && !backupFullID(id) {
			return usageError(stderr, "repository ID must be full 64 lowercase hexadecimal characters")
		}
		confirm := func(message string) bool {
			fmt.Fprintln(stderr, message)
			if yes {
				return true
			}
			answer := ""
			if ask("Proceed? [y/N]", &answer) != nil {
				return false
			}
			return answer == "y" || answer == "yes"
		}
		if !confirm(fmt.Sprintf("Register %s repository %s on %s (%s), prefix %s. No application backups or schedules will be created.", mode, name, server.Name, server.Target, prefix)) {
			return usageError(stderr, "repository setup not authorized")
		}
		var creds backupbackend.Credentials
		if fromStdin {
			data, e := io.ReadAll(io.LimitReader(input, backup.MaxPrivateBytes+1))
			if e != nil || len(data) > backup.MaxPrivateBytes {
				return usageError(stderr, "invalid private JSON input")
			}
			if !backupPrivateFields(data) {
				return usageError(stderr, "invalid private JSON input")
			}
			d := json.NewDecoder(strings.NewReader(string(data)))
			d.DisallowUnknownFields()
			if d.Decode(&creds) != nil {
				return usageError(stderr, "invalid private JSON input")
			}
			var extra any
			if !errors.Is(d.Decode(&extra), io.EOF) {
				return usageError(stderr, "expected one private JSON object")
			}
		} else {
			if !terminal {
				return usageError(stderr, "noninteractive setup requires --credentials-stdin")
			}
			for _, p := range []struct {
				label string
				value *string
			}{{"Access key ID", &creds.AccessKeyID}, {"Secret access key", &creds.SecretAccessKey}} {
				fmt.Fprintln(stderr, p.label+":")
				v, e := secret(ctx, input, stderr, false)
				if e != nil {
					return lifecycleError(stderr, errors.New("credential input failed"))
				}
				*p.value = string(v)
			}
			if mode == "connect" {
				fmt.Fprintln(stderr, "Existing repository password:")
				v, e := secret(ctx, input, stderr, false)
				if e != nil {
					return lifecycleError(stderr, errors.New("credential input failed"))
				}
				creds.Password = string(v)
			}
		}
		if mode == "new" {
			if creds.Password != "" {
				return usageError(stderr, "omit password for new repositories")
			}
			creds.Password, e = backupPassword()
			if e != nil {
				return lifecycleError(stderr, e)
			}
		}
		if e = backupbackend.ValidateCredentials(creds); e != nil {
			return usageError(stderr, "invalid repository credentials")
		}
		private, e = json.Marshal(creds)
		if e != nil {
			return lifecycleError(stderr, errors.New("credential encoding failed"))
		}
		if mode == "connect" && id == "" {
			observed, e := run(backup.Request{Action: "probe", Name: name, Config: &cfg}, private)
			if e != nil || observed.Repository == nil || !backupFullID(observed.Repository.RepositoryID) {
				return lifecycleError(stderr, errors.New("repository probe failed; no registration attempted"))
			}
			id = observed.Repository.RepositoryID
			fmt.Fprintf(stderr, "Observed repository ID: %s\n", id)
			answer := ""
			if ask("Confirm this full repository ID? [y/N]", &answer) != nil || (answer != "y" && answer != "yes") {
				return usageError(stderr, "repository identity not confirmed")
			}
			req.ExpectedID = id
		}
		kit = backup.RecoveryKit{Version: 1, Name: name, Operation: operation, Config: cfg, Credentials: creds, RepositoryID: id, Format: 2, ResticVersion: backupbackend.Version, Notice: "No application backups yet. Copy this kit to an independent password manager. Offline inspection: use restic cat config --no-cache --no-lock with this repository configuration and credentials."}
		staged, e = stageBackupKit(path, kit)
		if e != nil {
			return lifecycleError(stderr, errors.New("could not securely stage recovery kit; no registration attempted"))
		}
		defer staged.root.Close()
	}
	if e = backup.ValidateRequest(req); e != nil {
		return usageError(stderr, "invalid repository request")
	}
	result, e := run(req, private)
	if e != nil {
		fmt.Fprintf(stderr, "Repository outcome unknown: target %s (%s), name %s, operation %s. Keep the recovery kit. Inspect: vamos backup repository show --server %s %s\n", server.Name, server.Target, name, req.Operation, server.Name, name)
		if req.Operation != "" {
			fmt.Fprintf(stderr, "Resume: vamos backup repository resume --server %s %s %s\n", server.Name, name, req.Operation)
		}
		return 1
	}
	if staged != nil {
		if result.Repository == nil || !backupFullID(result.Repository.RepositoryID) || result.Repository.Name != name || result.Repository.Operation != req.Operation || (req.ExpectedID != "" && result.Repository.RepositoryID != req.ExpectedID) {
			fmt.Fprintf(stderr, "Registration may have completed; keep pending kit. Target %s, name %s, operation %s. Inspect with vamos backup repository show --server %s %s; resume with vamos backup repository resume --server %s %s %s\n", server.Target, name, req.Operation, server.Name, name, server.Name, name, req.Operation)
			return 1
		}
		kit.RepositoryID = result.Repository.RepositoryID
		if e = staged.update(kit); e != nil {
			return lifecycleError(stderr, errors.New("remote registration may have completed; recovery kit update failed; retain original key and inspect recorded metadata, do not repeat setup"))
		}
	}
	// Only typed metadata is emitted, never worker diagnostics or private inputs.
	if *asJSON {
		if e = json.NewEncoder(stdout).Encode(result); e != nil {
			return lifecycleError(stderr, e)
		}
	} else {
		fmt.Fprintln(stdout, "Repository metadata (no application backups or schedules created):")
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Fprintln(stdout, string(data))
	}
	return 0
}

// Read one byte at a time so public prompts never prefetch private input.
func backupPromptLine(r io.Reader) (string, error) {
	var line []byte
	var b [1]byte
	for len(line) < 4096 {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		if b[0] == '\n' {
			return string(line), nil
		}
		line = append(line, b[0])
	}
	return "", errors.New("answer too long")
}

func backupSafeResponse(r *backup.Response) bool {
	check := func(m backup.Metadata) bool {
		if !backup.ValidName(m.Name) || (m.Operation != "" && !backup.ValidOperation(m.Operation)) || (m.RepositoryID != "" && !backupFullID(m.RepositoryID)) {
			return false
		}
		if backupbackend.ValidateConfig(m.Config) != nil {
			return false
		}
		switch m.Mode {
		case "create", "connect", "probe":
		default:
			return false
		}
		switch m.State {
		case "prepared", "initializing", "ready", "unverified", "observed":
		default:
			return false
		}
		for _, stamp := range []string{m.CreatedAt, m.VerifiedAt} {
			if stamp != "" {
				if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
					return false
				}
			}
		}
		return m.Format == 0 || m.Format == 2
	}
	if r.Repository != nil && !check(*r.Repository) {
		return false
	}
	if len(r.Repositories) > backup.MaxRepositories {
		return false
	}
	for _, m := range r.Repositories {
		if !check(m) {
			return false
		}
	}
	return true
}

func backupFullID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Bind metadata to the operation before displaying it or finalizing a kit.
func backupResponseMatches(req backup.Request, r *backup.Response) bool {
	if req.Action == "list" {
		return r.Repository == nil
	}
	m := r.Repository
	if m == nil || len(r.Repositories) != 0 || m.Name != req.Name {
		return false
	}
	if req.Operation != "" && m.Operation != req.Operation {
		return false
	}
	if req.Config != nil && m.Config != *req.Config {
		return false
	}
	if req.ExpectedID != "" && m.RepositoryID != req.ExpectedID {
		return false
	}
	if req.Action == "create" || req.Action == "connect" || req.Action == "verify" || req.Action == "resume" {
		if m.State != "ready" || !backupFullID(m.RepositoryID) || m.Format != 2 {
			return false
		}
	}
	if req.Action == "probe" && m.State != "observed" {
		return false
	}
	if req.Action == "create" || req.Action == "connect" || req.Action == "probe" {
		if m.Mode != req.Action || !backupFullID(m.RepositoryID) || m.Format != 2 {
			return false
		}
	}
	return true
}

// encoding/json accepts duplicate keys and case-insensitive field aliases.
// Private input intentionally accepts neither.
func backupPrivateFields(data []byte) bool {
	d := json.NewDecoder(strings.NewReader(string(data)))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		tok, err = d.Token()
		if err != nil {
			return false
		}
		key, ok := tok.(string)
		if !ok || seen[key] {
			return false
		}
		switch key {
		case "access_key_id", "secret_access_key", "session_token", "password", "ca_certificate":
		default:
			return false
		}
		seen[key] = true
		value, err := d.Token()
		if _, ok := value.(string); err != nil || !ok {
			return false
		}
	}
	tok, err = d.Token()
	if err != nil || tok != json.Delim('}') {
		return false
	}
	_, err = d.Token()
	return errors.Is(err, io.EOF)
}
