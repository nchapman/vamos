package deploy

// One-off runs are deliberately noninteractive and never resumable. All mutation
// hooks must be called under Execute's app/global locks. Observation is read-only.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/nchapman/vamos/internal/buildcontext"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/secrets"
)

type RunResult struct {
	Operation       string `json:"operation"`
	Service         string `json:"service"`
	Phase           string `json:"phase"`
	Container       string `json:"container"`
	Image           string `json:"image"`
	ExitCode        *int   `json:"exit_code"`
	Output          string `json:"output,omitempty"`
	Error           string `json:"error,omitempty"`
	Purpose         string `json:"purpose,omitempty"`
	ParentOperation string `json:"parent_operation,omitempty"`
}

type runRecord struct {
	Version     int
	App, HostID string
	RunResult
	DockerID   string
	Config     config.Service
	SecretRefs map[string]secrets.Ref
}

const runRecordLimit = 128 << 10
const runOutputLimit = 8 << 10

func runPath(m *Manager, j *journal, op string) string {
	return filepath.Join(m.dir(j.App), op+".run.json")
}
func validateRun(j *journal, r *runRecord) error {
	if r == nil || !validRunVersion(r) || r.App != j.App || r.HostID != j.HostID || !operation.MatchString(r.Operation) || !label.MatchString(r.Service) || len(r.Service) > 63 || r.Container != "vamos-run-"+r.Operation || len(r.Config.Command) == 0 || !matchingSecretRefs(r.SecretRefs, r.Config.Secrets) || len(r.Output) > runOutputLimit || r.Config.HTTP != nil || r.Config.Volume != nil {
		return errors.New("invalid run record")
	}
	if err := ValidateApp(&config.App{Name: r.App, Services: map[string]config.Service{r.Service: r.Config}}); err != nil {
		return errors.New("invalid captured run configuration")
	}
	if r.Config.Command[0] == "" || (r.ExitCode != nil && (*r.ExitCode < 0 || *r.ExitCode > 255)) {
		return errors.New("invalid run command or exit code")
	}
	if r.Phase == "building" && (r.Image != "" || r.DockerID != "") {
		return errors.New("invalid building run identity")
	}
	total := 0
	for _, arg := range r.Config.Command {
		total += len(arg)
	}
	if total > 32<<10 {
		return errors.New("run command exceeds total limit")
	}
	switch r.Phase {
	case "building", "create-intent", "start-intent", "complete":
	default:
		return errors.New("invalid run phase")
	}
	if r.Phase != "building" && !imageID.MatchString(r.Image) {
		return errors.New("invalid run image")
	}
	if (r.Phase == "start-intent" || r.Phase == "complete") && len(r.DockerID) != 64 {
		return errors.New("missing run container identity")
	}
	if len(r.Error) > 1024 || len(r.Config.Command) > 256 {
		return errors.New("invalid run bounds")
	}
	for _, arg := range r.Config.Command {
		if len(arg) > 8192 || strings.ContainsRune(arg, 0) {
			return errors.New("invalid run command")
		}
	}
	if r.DockerID != "" {
		for _, c := range r.DockerID {
			if !strings.ContainsRune("0123456789abcdef", c) {
				return errors.New("invalid Docker identity")
			}
		}
		if len(r.DockerID) != 64 {
			return errors.New("invalid Docker identity")
		}
	}
	if !validSecretRefs(r.SecretRefs) {
		return errors.New("invalid run secrets")
	}
	if (r.Phase == "complete") != (r.ExitCode != nil) {
		return errors.New("invalid run outcome")
	}
	return nil
}
func (m *Manager) loadRun(j *journal, op string) (*runRecord, error) {
	if j == nil || !operation.MatchString(op) {
		return nil, errors.New("invalid run operation")
	}
	p := runPath(m, j, op)
	if safe(p) != nil {
		return nil, errors.New("unsafe run record")
	}
	st, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !runPrivate(st) || st.Size() > runRecordLimit {
		return nil, errors.New("unsafe run record")
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, errors.New("read run record failed")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) || !runPrivate(opened) || opened.Size() > runRecordLimit {
		return nil, errors.New("run record changed or unsafe")
	}
	b, err := io.ReadAll(io.LimitReader(f, runRecordLimit+1))
	if err != nil || len(b) > runRecordLimit {
		return nil, errors.New("read run record failed")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var r runRecord
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid run record JSON")
	}
	canonical, _ := json.Marshal(&r)
	if !bytes.Equal(b, canonical) {
		return nil, errors.New("noncanonical run record")
	}
	if err = validateRun(j, &r); err != nil {
		return nil, err
	}
	if r.Operation != op {
		return nil, errors.New("run operation mismatch")
	}
	return &r, nil
}
func (m *Manager) saveRun(j *journal, r *runRecord) error {
	if err := validateRun(j, r); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil || len(b) > runRecordLimit {
		return errors.New("run record exceeds limit")
	}
	if atomic(runPath(m, j, r.Operation), b, 0600) != nil {
		return errors.New("persist run record failed")
	}
	return nil
}

// verifyRunEndpoint never admits an endpoint by labels alone, including during
// the create/ID-save crash window. That window requires manual reconciliation.
func (m *Manager) verifyRunEndpoint(ctx context.Context, j *journal, v *inspection) error {
	r, err := m.loadRun(j, v.Config.Labels["vamos.run"])
	if err != nil {
		return errors.New("unrecorded run endpoint")
	}
	return m.verifyRun(ctx, j, r, v)
}
func (m *Manager) verifyRun(ctx context.Context, j *journal, r *runRecord, v *inspection) error {
	bad := errors.New("run endpoint identity or configuration mismatch")
	if err := verifyResources(v, r.Config.Resources); err != nil {
		return err
	}
	if r.DockerID == "" || v.ID != r.DockerID || strings.TrimPrefix(v.Name, "/") != r.Container || v.Config.Image != r.Image || len(v.Config.Volumes) != 0 || len(v.Mounts) != 0 || len(v.HostConfig.Mounts) != 0 || unsafeMultiHostConfig(v) || v.HostConfig.RestartPolicy.Name != "no" || v.HostConfig.NetworkMode != network(j) || len(v.NetworkSettings.Networks) != 1 || len(v.HostConfig.PortBindings) != 0 {
		return bad
	}
	for _, p := range v.NetworkSettings.Ports {
		if len(p) != 0 {
			return bad
		}
	}
	aliases, err := endpointAliases(v, network(j))
	if err != nil {
		return bad
	}
	for _, a := range aliases {
		if a != r.Container && a != v.ID && a != v.ID[:12] {
			return bad
		}
	}
	im, err := m.inspect(ctx, "image", r.Image)
	if err != nil || im.ID != r.Image || len(im.Config.Volumes) != 0 {
		return bad
	}
	labels := map[string]string{}
	for k, val := range im.Config.Labels {
		labels[k] = val
	}
	labels["vamos.host"] = j.HostID
	labels["vamos.app"] = j.App
	labels["vamos.operation"] = r.Operation
	labels["vamos.service"] = r.Service
	labels["vamos.run"] = r.Operation
	if r.Purpose == "release" {
		labels["vamos.release"] = r.ParentOperation
	}
	env := map[string]string{}
	for _, x := range im.Config.Env {
		k, val, ok := strings.Cut(x, "=")
		if !ok {
			return bad
		}
		env[k] = val
	}
	for k, val := range r.Config.Env {
		env[k] = val
	}
	values, err := m.secretValues(j, r.SecretRefs)
	if err != nil {
		return bad
	}
	for k, val := range values {
		env[k] = val
	}
	actual := map[string]string{}
	for _, x := range v.Config.Env {
		k, val, ok := strings.Cut(x, "=")
		if !ok {
			return bad
		}
		if _, ok = actual[k]; ok {
			return bad
		}
		actual[k] = val
	}
	if !reflect.DeepEqual(labels, v.Config.Labels) || !reflect.DeepEqual(env, actual) || !reflect.DeepEqual(r.Config.Command, v.Config.Cmd) || !reflect.DeepEqual(im.Config.Entrypoint, v.Config.Entrypoint) || im.Config.User != v.Config.User || im.Config.WorkingDir != v.Config.WorkingDir {
		return bad
	}
	return nil
}

func (m *Manager) runStatus(ctx context.Context, j *journal, op string) (*RunResult, error) {
	r, err := m.loadRun(j, op)
	if err != nil {
		return nil, err
	}
	return m.observeRun(ctx, j, r), nil
}
func (m *Manager) observeRun(ctx context.Context, j *journal, r *runRecord) *RunResult {
	out := r.RunResult
	if r.Phase == "complete" {
		return &out
	}
	if r.Phase == "building" || r.Phase == "create-intent" {
		out.Phase = "not-started"
		if out.Error == "" {
			out.Error = "start was not requested; worker may still be preparing"
		}
		return &out
	}
	out.Phase = "unknown"
	out.Error = "execution outcome unknown; run will not be retried"
	v, err := m.inspect(ctx, "container", r.Container)
	if err != nil || m.verifyRun(ctx, j, r, v) != nil {
		return &out
	}
	if r.Phase != "start-intent" {
		return &out
	}
	if v.State.Running || v.State.Restarting || v.State.Paused {
		out.Phase = "running"
		out.Error = ""
		return &out
	}
	// Decode State separately: ordinary deployment inspection need not expose exit status.
	b, err := m.cmd(ctx, "docker", "container", "inspect", r.DockerID)
	if err != nil {
		return &out
	}
	var rows []struct {
		ID    string `json:"Id"`
		State struct {
			Status     string
			ExitCode   *int
			FinishedAt string
		}
	}
	if json.Unmarshal(b, &rows) != nil || len(rows) != 1 || rows[0].ID != r.DockerID || rows[0].State.Status != "exited" || rows[0].State.ExitCode == nil || *rows[0].State.ExitCode < 0 || *rows[0].State.ExitCode > 255 || rows[0].State.FinishedAt == "" || strings.HasPrefix(rows[0].State.FinishedAt, "0001-") {
		return &out
	}
	out.Phase = "complete"
	out.ExitCode = rows[0].State.ExitCode
	out.Error = ""
	out.Output, _ = m.runLogs(ctx, j, r.Operation)
	return &out
}
func (m *Manager) runLogs(ctx context.Context, j *journal, op string) (string, error) {
	r, err := m.loadRun(j, op)
	if err != nil {
		return "", err
	}
	if r.Phase == "building" && r.Error != "" {
		return r.Error, nil
	}
	v, err := m.inspect(ctx, "container", r.Container)
	if err != nil && r.Phase == "complete" && m.runContainerAbsent(ctx, r.Container) {
		return r.Output, nil
	}
	if err != nil || m.verifyRun(ctx, j, r, v) != nil {
		return "", errors.New("run logs identity unavailable")
	}
	result, err := m.Run(ctx, "docker", []string{"logs", "--tail", "100", r.DockerID}, nil)
	if err != nil {
		return "", errors.New("read run logs failed")
	}
	b := append(result.Stdout, result.Stderr...)
	if len(b) > runOutputLimit {
		b = b[len(b)-runOutputLimit:]
	}
	return strings.ToValidUTF8(string(b), "?"), nil
}

// runContainerAbsent distinguishes a missing container from an inspection failure.
func (m *Manager) runContainerAbsent(ctx context.Context, name string) bool {
	b, err := m.cmd(ctx, "docker", "container", "ls", "--format", "{{json .}}", "--all")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var row struct{ Names string }
		if json.Unmarshal([]byte(line), &row) != nil || row.Names == "" {
			return false
		}
		for _, n := range strings.Split(row.Names, ",") {
			if strings.TrimPrefix(strings.TrimSpace(n), "/") == name {
				return false
			}
		}
	}
	return true
}

func (m *Manager) preflightRuns(ctx context.Context, j *journal, allowedHook ...string) error {
	if j == nil {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(m.dir(j.App), "*.run.json"))
	if err != nil {
		return err
	}
	if len(paths) > 256 {
		return errors.New("run history limit reached")
	}
	for _, p := range paths {
		r, err := m.loadRun(j, strings.TrimSuffix(filepath.Base(p), ".run.json"))
		if err != nil {
			return err
		}
		if len(allowedHook) == 1 && allowedHook[0] != "" && r.Operation == allowedHook[0] {
			if j.Phase != "releasing" || j.Hook == nil || r.Operation != j.Hook.Operation || !matchingHookRecord(m.hookRecord(j), r) {
				return errors.New("owning release execution does not match captured deployment")
			}
			continue // Only its owner may resume observation; no general retry permission.
		}
		v, err := m.inspect(ctx, "container", r.Container)
		if err != nil {
			if (r.Phase == "building" || r.Phase == "complete" || m.plannedRunRemoval(j, r)) && m.runContainerAbsent(ctx, r.Container) {
				continue
			}
			return errors.New("unknown run blocks mutation")
		}
		if v.State.Running || v.State.Restarting || v.State.Paused {
			return errors.New("live run blocks mutation")
		}
		// No adoption after start intent. Prestart adoption only captures identity for cleanup.
		if r.Phase == "create-intent" && r.DockerID == "" && len(v.ID) == 64 && (v.State.StartedAt == "" || strings.HasPrefix(v.State.StartedAt, "0001-")) {
			candidate := *r
			candidate.DockerID = v.ID
			if validateRun(j, &candidate) == nil && m.verifyRun(ctx, j, &candidate, v) == nil {
				if err := m.saveRun(j, &candidate); err != nil {
					return err
				}
				r = &candidate
			}
		}
		if m.verifyRun(ctx, j, r, v) != nil {
			return errors.New("unsafe run blocks mutation")
		}
		if r.Phase == "create-intent" && (v.State.StartedAt == "" || strings.HasPrefix(v.State.StartedAt, "0001-")) {
			continue
		}
		if r.Phase != "complete" {
			observed := m.observeRun(ctx, j, r)
			if observed.Phase != "complete" {
				return errors.New("active or unknown run blocks mutation")
			}
			// A worker may have exited after the command but before its final save.
			// Reconcile evidence under the mutation lock; never execute it again.
			r.RunResult = *observed
			if err := m.saveRun(j, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) executeRun(ctx context.Context, r Request, data []byte, j *journal, h *lifecycle.State, state *secrets.State) (Response, error) {
	resp := Response{Version: 1}
	if !isMulti(j) || !hasCurrent(j) || j.HostID != h.ID {
		return resp, errors.New("run requires bootstrapped multi-service app")
	}
	if prior, err := m.loadRun(j, r.Operation); err == nil {
		resp.Run = m.observeRun(ctx, j, prior)
		return resp, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return resp, err
	}
	if err := m.verifyMultiHost(); err != nil {
		return resp, err
	}
	if _, err := os.Lstat(filepath.Join(m.dir(j.App), r.Operation+".context.tar")); !errors.Is(err, os.ErrNotExist) {
		return resp, errors.New("run operation context provenance already exists")
	}
	if j.Operation == r.Operation || retainedOperation(j, r.Operation) || j.LastMaintenanceOperation == r.Operation {
		return resp, errors.New("operation ID already used")
	}
	if j.Phase != "ready" || j.Maintenance != nil {
		return resp, errors.New("incomplete app operation blocks run")
	}
	paths, scanErr := filepath.Glob(filepath.Join(m.dir(j.App), "*.run.json"))
	if scanErr != nil || len(paths) >= 256 {
		return resp, errors.New("run history full; explicit cleanup required")
	}
	// An IID survives record cleanup and prevents reuse of that operation ID.
	used, scanErr := filepath.Glob(filepath.Join(m.dir(j.App), r.Operation+".*.image.iid"))
	if scanErr != nil || len(used) != 0 {
		return resp, errors.New("run operation image provenance already exists")
	}
	if err := m.preflightRuns(ctx, j); err != nil {
		return resp, err
	}
	nw, err := m.inspect(ctx, "network", network(j))
	if err != nil {
		return resp, errors.New("existing app network required")
	}
	if err = m.validateAppNetwork(ctx, j, nw); err != nil {
		return resp, errors.New("app network ownership mismatch")
	}
	if r.Config == nil {
		return resp, errors.New("run configuration required")
	}
	cfg, ok := r.Config.Services[r.Service]
	if !ok || len(r.Command) == 0 {
		return resp, errors.New("run service and command required")
	}
	if err := m.requireResourceSupport(ctx, cfg.Resources); err != nil {
		return resp, err
	}
	selected := &config.App{Name: j.App, Services: map[string]config.Service{r.Service: cfg}}
	archives, err := buildcontext.ReadBundle(data, selected)
	if err != nil {
		return resp, errors.New("invalid selected run bundle")
	}
	refs, err := state.Snapshot(cfg.Secrets)
	if err != nil {
		return resp, errors.New("selected run secrets unavailable")
	}
	cfg.Command = append([]string(nil), r.Command...)
	cfg.HTTP = nil
	cfg.Volume = nil
	rec := &runRecord{Version: 1, App: j.App, HostID: j.HostID, RunResult: RunResult{Operation: r.Operation, Service: r.Service, Phase: "building", Container: "vamos-run-" + r.Operation}, Config: cfg, SecretRefs: refs}
	if err = m.saveRun(j, rec); err != nil {
		return resp, err
	}
	// This build-only path intentionally never calls save(j) or writes a context archive.
	tmp := &journal{Status: Status{App: j.App, Operation: r.Operation}, HostID: j.HostID}
	if cfg.Build == nil {
		rec.Image, err = m.resolvePrebuilt(ctx, tmp, cfg)
	} else {
		key := imageKey(cfg)
		p := filepath.Join(m.dir(j.App), r.Operation+"."+key+".image.iid")
		if safe(p) != nil {
			return resp, errors.New("unsafe run image file")
		}
		if _, e := os.Lstat(p); !errors.Is(e, os.ErrNotExist) {
			return resp, errors.New("run image identity already exists")
		}
		out, buildErr := m.buildImage(ctx, j.HostID, cfg.Build.Dockerfile, p, archives[key])
		if buildErr != nil {
			rec.Error = m.buildDiagnostic(j, out, buildErr, 1024, len(rec.SecretRefs) > 0)
			if saveErr := m.saveRun(j, rec); saveErr != nil {
				return resp, errors.New("build failed; diagnostic persistence failed")
			}
			resp.Run = m.observeRun(ctx, j, rec)
			return resp, errors.New(rec.Error)
		}
		if err == nil {
			var b []byte
			st, statErr := os.Lstat(p)
			if statErr != nil || !st.Mode().IsRegular() || st.Size() > 128 || safe(p) != nil {
				return resp, errors.New("invalid run image identity file")
			}
			b, err = os.ReadFile(p)
			rec.Image = strings.TrimSpace(string(b))
			if err == nil {
				err = atomic(p, []byte(rec.Image+"\n"), 0600)
			}
		}
	}
	if err != nil || !imageID.MatchString(rec.Image) {
		return resp, errors.New("run image resolution failed")
	}
	im, err := m.inspect(ctx, "image", rec.Image)
	if err != nil || im.ID != rec.Image || imageVolumesAllowed(im, nil, false) != nil {
		return resp, errors.New("unsafe run image")
	}
	rec.Phase = "create-intent"
	if err = m.saveRun(j, rec); err != nil {
		return resp, err
	}
	args := runCreateArgs(j, rec)
	rel := &release{Image: rec.Image, SecretRefs: refs}
	if m.runContainer(ctx, j, rel, args) != nil {
		return resp, errors.New("run create outcome unknown; no retry")
	}
	v, err := m.inspect(ctx, "container", rec.Container)
	if err != nil {
		return resp, errors.New("run create identity unavailable")
	}
	rec.DockerID = v.ID
	if len(v.ID) != 64 || m.verifyRun(ctx, j, rec, v) != nil || v.State.Running || v.State.Restarting {
		return resp, errors.New("unsafe created run")
	}
	rec.Phase = "start-intent"
	if err = m.saveRun(j, rec); err != nil {
		return resp, err
	}
	// Never repeat start, even if Docker applied it but its response was lost.
	if _, err = m.cmd(ctx, "docker", "start", rec.DockerID); err == nil {
		_, _ = m.cmd(ctx, "docker", "wait", rec.DockerID)
	}
	resp.Run = m.observeRun(ctx, j, rec)
	if resp.Run.Phase == "complete" {
		rec.RunResult = *resp.Run
		if m.saveRun(j, rec) != nil {
			resp.Run.Error = "result persistence uncertain; observe status; do not retry"
		}
	}
	return resp, nil
}
