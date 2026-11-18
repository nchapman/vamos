package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"time"

	"github.com/nchapman/vamos/internal/backup"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/ssh"
)

const backupPlanHelp = `Usage:
  vamos backup configure --server NAME --app APP -f backup.yaml [--yes] [--json]
  vamos backup plan --server NAME --app APP [--json]
Flags must precede arguments; positional arguments are not accepted.
--app is required. -f defaults to backup.yaml.
--server defaults to the registered default, not project bindings.
Both commands accept --executor PATH and --timeout DURATION (default 5m).
Configure previews by default. --yes previews then applies that exact revision
and binding digest using compare-and-swap. Apply replaces the whole app plan;
omitted sources/sets are removed from policy, unlike a scoped deploy.
No credential values are accepted; PostgreSQL uses secret-backed selectors.
Set names do not create schedules. A stop list grants no downtime permission.
Preview prints current/proposed JSON; --json suppresses the explanatory heading.
Plan reads recorded metadata from the host, not current capture readiness.
See docs/backup-plans.md for fields, examples and selection limits.
Capture, restore and schedules are not implemented. No application backups are created.
`

func backupPlanCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps appDeps) int {
	if len(args) == 0 || (args[0] != "configure" && args[0] != "plan") {
		return usageError(stderr, "expected backup configure or plan")
	}
	action := args[0]
	f := flag.NewFlagSet("backup "+action, flag.ContinueOnError)
	serverName := f.String("server", "", "registered server")
	app := f.String("app", "", "explicit app")
	executor := f.String("executor", "", "executor artifact")
	timeout := f.Duration("timeout", 5*time.Minute, "deadline")
	asJSON := f.Bool("json", false, "structured metadata")
	var filename string
	var yes bool
	if action == "configure" {
		f.StringVar(&filename, "f", "backup.yaml", "backup plan file")
		f.BoolVar(&yes, "yes", false, "approve fresh preview")
	}
	f.Usage = func() { fmt.Fprint(f.Output(), backupPlanHelp) }
	if err := parseFlags(f, args[1:], stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if f.NArg() != 0 || !backup.ValidName(*app) || *timeout <= 0 {
		return usageError(stderr, "an explicit valid --app and flags only are required")
	}
	var plan *backup.Plan
	if action == "configure" {
		// Check before opening so special files cannot block, then check the opened file too.
		info, err := os.Stat(filename)
		if err != nil || !info.Mode().IsRegular() || info.Size() > backup.MaxPlanBytes {
			return usageError(stderr, "invalid backup plan file")
		}
		file, err := os.Open(filename)
		if err != nil {
			return usageError(stderr, "cannot read backup plan file")
		}
		info, err = file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > backup.MaxPlanBytes {
			file.Close()
			return usageError(stderr, "invalid backup plan file")
		}
		p, err := backup.DecodePlan(file)
		file.Close()
		if err != nil {
			// DecodePlan returns only fixed, value-free local diagnostics. Do not
			// apply this policy to raw parser, filesystem or remote errors.
			return usageError(stderr, err.Error())
		}
		if p.App != *app {
			return usageError(stderr, "backup plan app does not match --app")
		}
		plan = &p
	}
	store, err := deps.Registry()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	state, err := store.Read()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	server, err := selectAppServer(state, "", "", *serverName)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	if err = ssh.ValidateTarget(server.Target); err != nil {
		return lifecycleError(stderr, err)
	}
	hostConfig := lifecycle.Config{Name: server.Name, Domain: server.Domain}
	if err = lifecycle.ValidateConfig(hostConfig); err != nil {
		return lifecycleError(stderr, err)
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var admissionMessage string
	fail := func(apply bool) int {
		if admissionMessage != "" {
			fmt.Fprintln(stderr, admissionMessage)
		} else if apply {
			fmt.Fprintf(stderr, "Backup plan apply outcome is unknown or refused. Inspect recorded metadata with: vamos backup plan --server %s --app %s\n", server.Name, *app)
		} else {
			fmt.Fprintln(stderr, "Backup plan observation failed; no apply requested.")
		}
		return 1
	}
	run := func(r backup.PlanRequest) (*backup.PlanResponse, bool) {
		response, e := runAppTransport(ctx, server.Target, *executor, deploy.Request{Version: 1, Action: "backup-plan", App: *app, Host: hostConfig, BackupPlan: &r}, nil, deps)
		if admissionMessage = privateAdmissionMessage(e); admissionMessage != "" {
			return nil, false
		}
		if response.ErrorCode != "" {
			if message := deploy.RefusalMessage("backup-plan", response.ErrorCode); message != "" {
				fmt.Fprintln(stderr, message)
			}
			return nil, false
		}
		if e != nil || response.Version != 1 || response.Error != "" || response.BackupPlan == nil {
			return nil, false
		}
		return response.BackupPlan, true
	}
	request := backup.PlanRequest{Action: "show"}
	if plan != nil {
		request = backup.PlanRequest{Action: "preview", Plan: plan}
	}
	response, ok := run(request)
	if !ok || !backupPlanObservation(response, *app, plan) {
		return fail(false)
	}
	// Remote warning prose is not trusted output. Publish a local, fixed warning.
	safeWarnings := []string{
		"Plan configuration only; capture, restore and schedules are not implemented.",
		"Source prerequisites, database authentication and repository access are not verified by plan setup.",
		"Coverage is unclassified; future capture must report omitted sources and persistent stores.",
	}
	response.Warnings = safeWarnings
	if yes {
		preview := response
		response, ok = run(backup.PlanRequest{Action: "apply", Plan: plan, ExpectedRevision: preview.CurrentRevision, ExpectedDigest: preview.Digest})
		if !ok || !response.Applied || response.Current == nil || response.Current.UpdatedAt == "" || !backupPlanRecordValid(response.Current, *app) || response.CurrentRevision != preview.Digest || response.Current.Revision != preview.Digest || response.Current.HostID != preview.Proposed.HostID || !reflect.DeepEqual(response.Current.Plan, preview.Proposed.Plan) || !reflect.DeepEqual(response.Current.Bindings, preview.Proposed.Bindings) || response.Proposed != nil {
			return fail(true)
		}
		response.Warnings = safeWarnings
	}
	if !*asJSON {
		fmt.Fprintln(stdout, "Backup plan recorded metadata (no application backup or schedule created):")
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if enc.Encode(response) != nil {
		return 1
	}
	return 0
}

var backupPlanHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func backupPlanRecordValid(r *backup.PlanRecord, app string) bool {
	return r != nil && r.Plan.App == app && backup.ValidatePlanRecord(*r) == nil
}

func backupPlanObservation(r *backup.PlanResponse, app string, plan *backup.Plan) bool {
	if r == nil || r.Applied {
		return false
	}
	if r.Current == nil {
		if r.CurrentRevision != "none" {
			return false
		}
	} else if r.Current.UpdatedAt == "" || !backupPlanRecordValid(r.Current, app) || r.CurrentRevision != r.Current.Revision {
		return false
	}
	if plan == nil {
		return r.Proposed == nil && r.Digest == ""
	}
	if !backupPlanRecordValid(r.Proposed, app) || r.Digest != r.Proposed.Revision || !backupPlanHash.MatchString(r.Digest) || !reflect.DeepEqual(r.Proposed.Plan, *plan) {
		return false
	}
	return r.Current == nil || r.Current.HostID == r.Proposed.HostID
}
