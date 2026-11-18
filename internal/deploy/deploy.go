// Package deploy implements recoverable app deployments and recorded one-off commands.
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/appstate"
	"github.com/nchapman/vamos/internal/backup"
	"github.com/nchapman/vamos/internal/buildcontext"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/engine"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
	"github.com/nchapman/vamos/internal/secrets"
)

type Request struct {
	BackupPlan     *backup.PlanRequest `json:"backup_plan,omitempty"`
	Backup         *backup.Request     `json:"backup,omitempty"`
	Partial        bool                `json:"partial,omitempty"`
	Bindings       map[string]string   `json:"bindings,omitempty"`
	HostIdentity   string              `json:"host_identity,omitempty"`
	ReAdd          bool                `json:"re_add,omitempty"`
	AllowProtected bool                `json:"allow_protected,omitempty"`
	AllowDowntime  bool                `json:"allow_downtime,omitempty"`
	Command        []string            `json:"command,omitempty"`
	RunOperation   string              `json:"run_operation,omitempty"`
	ReuseData      bool                `json:"reuse_data,omitempty"`
	ContextFormat  string              `json:"context_format,omitempty"`
	Service        string              `json:"service,omitempty"`
	Candidate      bool                `json:"candidate,omitempty"`
	Replica        int                 `json:"replica,omitempty"`
	SecretName     string              `json:"secret_name,omitempty"`
	SecretNames    []string            `json:"secret_names,omitempty"`
	Apply          bool                `json:"apply,omitempty"`
	Supersede      string              `json:"supersede,omitempty"`
	Version        int                 `json:"version"`
	Action         string              `json:"action"`
	App            string              `json:"app"`
	Operation      string              `json:"operation,omitempty"`
	Config         *config.App         `json:"config,omitempty"`
	Host           lifecycle.Config    `json:"host"`
	Tail           int                 `json:"tail,omitempty"`
}
type Response struct {
	SetApplied      bool                 `json:"secret_applied,omitempty"`
	BackupPlan      *backup.PlanResponse `json:"backup_plan,omitempty"`
	Backup          *backup.Response     `json:"backup,omitempty"`
	ErrorCode       string               `json:"error_code,omitempty"`
	Apps            *AppList             `json:"apps,omitempty"`
	RollbackPreview *RollbackPreview     `json:"rollback_preview,omitempty"`
	Run             *RunResult           `json:"run,omitempty"`
	DeployPreview   *DeploymentPreview   `json:"deploy_preview,omitempty"`
	LogService      string               `json:"log_service,omitempty"`
	LogContainer    string               `json:"log_container,omitempty"`
	LogCandidate    bool                 `json:"log_candidate,omitempty"`
	Secret          *secrets.Entry       `json:"secret,omitempty"`
	SecretVersions  []secrets.Entry      `json:"secret_versions,omitempty"`
	Plan            *Plan                `json:"plan,omitempty"`
	Version         int                  `json:"version"`
	Status          *Status              `json:"status,omitempty"`
	Logs            string               `json:"logs,omitempty"`
	Error           string               `json:"error,omitempty"`
}
type SecretStatus struct {
	AppliedVersions map[string]string `json:"applied_versions,omitempty"`
	Name            string            `json:"name"`
	StagedVersion   string            `json:"staged_version"`
	AppliedVersion  string            `json:"applied_version,omitempty"`
	Pending         bool              `json:"pending"`
	Services        []string          `json:"services"`
}

type VolumeStatus struct {
	Service            string `json:"service"`
	Name               string `json:"name"`
	Mount              string `json:"mount"`
	ID                 string `json:"id"`
	State              string `json:"state"`
	Missing            bool   `json:"missing"`
	DataMayHaveChanged bool   `json:"data_may_have_changed"`
}

type Status struct {
	HostIdentity string                   `json:"host_identity,omitempty"`
	Run          *RunResult               `json:"run,omitempty"`
	Release      *RunResult               `json:"release,omitempty"`
	Volumes      []VolumeStatus           `json:"volumes,omitempty"`
	Services     map[string]ServiceStatus `json:"services,omitempty"`
	Secrets      []SecretStatus           `json:"secrets,omitempty"`
	App          string                   `json:"app"`
	URL          string                   `json:"url"`
	Phase        string                   `json:"phase"`
	Operation    string                   `json:"operation"`
	Active       bool                     `json:"active"`
	Container    string                   `json:"container"`
	LastError    string                   `json:"last_error"`
	Running      bool                     `json:"running"`
}
type releaseInstance struct {
	Ordinal     int
	Container   string
	Port        string
	PublishPort string
}

type release struct {
	Instances       []releaseInstance      `json:",omitempty"`
	VolumeID        string                 `json:",omitempty"`
	ExecutionIntent bool                   `json:",omitempty"`
	Service         string                 `json:",omitempty"`
	Config          *config.Service        `json:",omitempty"`
	SecretRefs      map[string]secrets.Ref `json:",omitempty"`
	Container       string
	Image           string
	Port            string
	PublishPort     string
	Operation       string
}
type DeploymentPreview struct {
	Service           string `json:"service"`
	CurrentImage      string `json:"current_image"`
	DesiredImage      string `json:"desired_image"`
	Downtime          bool   `json:"downtime"`
	NeedsConfirmation bool   `json:"needs_confirmation"`
}

type journal struct {
	SecretUpdate                 *secretUpdateCapture     `json:",omitempty"`
	Hook                         *releaseCapture          `json:",omitempty"`
	Selected                     []string                 `json:",omitempty"`
	Removed                      map[string]string        `json:",omitempty"`
	Rollback                     *rollbackCapture         `json:",omitempty"`
	Scope                        string                   `json:",omitempty"`
	ReuseData                    bool                     `json:",omitempty"`
	ServiceStates                map[string]*serviceState `json:",omitempty"`
	BuildImages                  map[string]string        `json:",omitempty"`
	BundleSHA                    string                   `json:",omitempty"`
	ServiceOrder                 []string                 `json:",omitempty"`
	ActiveService                string                   `json:",omitempty"`
	Completed, PreviousCompleted *completedRelease        `json:",omitempty"`
	SecretRefs                   map[string]secrets.Ref   `json:",omitempty"`
	Maintenance                  *maintenance             `json:",omitempty"`
	LastMaintenanceOperation     string                   `json:",omitempty"`
	LastMaintenanceAction        string                   `json:",omitempty"`
	Version                      int                      `json:"version"`
	Status
	Archive                      string
	HostID                       string
	Config                       *config.App
	Current, Previous, Candidate *release
	OldCaddy, NewCaddy           string
}
type Runner func(context.Context, string, []string, []byte) (process.Result, error)
type Manager struct {
	Builder           ImageBuilder
	RepositoryBackend backup.Backend
	DomainProbe       func(context.Context, string) DomainStatus
	// RuntimeDir is the private worker staging directory for transient env files.
	RuntimeDir string
	// AllocatePort selects an advisory loopback reservation. Docker's bind is authoritative.
	AllocatePort func() (string, error)
	// MutationLock overrides the default host-wide flock when a coordinator
	// manages per-app locks and concurrency governance.
	MutationLock func(ctx context.Context, app string, mut bool) (active bool, unlock func(), err error)
	// IngressLock serializes short Caddyfile and live-route transactions when
	// multiple apps deploy concurrently.
	IngressLock func(ctx context.Context) (func(), error)
	// Snapshotter overrides the BuildKit snapshotter choice ("auto" or
	// "native"). Empty defers to the VAMOS_BUILDKIT_SNAPSHOTTER environment
	// variable so operators can keep configuring hosts through systemd.
	Snapshotter    string
	Root           string
	Run            Runner
	HTTP           *http.Client
	HealthTimeout  time.Duration
	PrivateObserve time.Duration
	Drain          time.Duration
}

func (m *Manager) engine() *engine.Client {
	dir := m.RuntimeDir
	if dir == "" {
		dir = m.path("/run")
	}
	return &engine.Client{
		Run:          m.cmd,
		HTTP:         m.HTTP,
		AllocatePort: m.AllocatePort,
		RuntimeDir:   dir,
	}
}

func (m *Manager) lockIngress(ctx context.Context) (func(), error) {
	if m.IngressLock == nil {
		return func() {}, nil
	}
	unlock, err := m.IngressLock(ctx)
	if err != nil {
		return nil, err
	}
	if unlock == nil {
		return func() {}, nil
	}
	return unlock, nil
}

func allocatePort() (string, error) {
	return engine.AllocateLoopbackPort()
}

func NewManager() *Manager {
	return &Manager{Root: "/", Run: func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
		dockerArgs := dockerCommandArgs(n, a)
		if (len(dockerArgs) > 1 && dockerArgs[0] == "buildx" && dockerArgs[1] == "build") || (len(dockerArgs) > 0 && dockerArgs[0] == "logs") {
			return process.RunTail(c, n, a, b)
		}
		// Full Docker inspection includes runtime values, which can be much larger
		// than the metadata-only SSH response. Never increase the transport budget.
		if len(dockerArgs) > 1 && dockerArgs[1] == "inspect" {
			return process.RunLimited(c, n, a, b, 8<<20)
		}
		return process.Run(c, n, a, b)
	}, HTTP: &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, HealthTimeout: 30 * time.Second, PrivateObserve: 2 * time.Second, Drain: 2 * time.Second}
}

var label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
var operation = regexp.MustCompile(`^[0-9a-f]{32}$`)
var imageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func ValidateApp(a *config.App) error {
	if a == nil {
		return errors.New("config required")
	}
	for name, s := range a.Services {
		s.NormalizeReplicas()
		a.Services[name] = s
	}
	return a.Validate()
}

func validateLegacyApp(a *config.App) error {
	if err := ValidateApp(a); err != nil {
		return err
	}
	if a.Release != nil {
		return errors.New("legacy protocol does not support release commands")
	}
	for _, service := range a.Services {
		if service.Resources != nil {
			return errors.New("legacy protocol does not support resources; use a current client with a captured build bundle")
		}
		if h := service.HTTP; h != nil && (len(h.RateLimit) != 0 || len(h.Redirects) != 0) {
			return errors.New("rate limits and redirects require a current client with a captured build bundle")
		}
	}
	s, ok := a.Services["web"]
	if !ok || len(a.Services) != 1 || s.HTTP == nil || s.Volume != nil || len(s.Ports) != 0 || s.Build == nil || s.ReplicaCount() != 1 || s.Update == config.UpdateManual || len(s.HTTP.Domains) != 0 {
		return errors.New("legacy protocol supports only one public web service; use a current client with a captured build bundle")
	}
	return nil
}
func (r Request) Mutating() bool {
	if r.Action == "backup-plan" {
		return r.BackupPlan != nil && r.BackupPlan.Action != "show"
	}
	if r.Action == "backup-repository" {
		return r.Backup != nil && r.Backup.Mutating()
	}
	return r.Action == "service-remove" || r.Action == "rollback" || r.Action == "run" || r.Action == "deploy" || r.Action == "resume" || r.Action == "cleanup" || r.Action == "destroy" || r.Action == "secrets-stage" || r.Action == "secrets-set"
}

func ValidateRequest(r Request) error {
	if r.Action == "backup-plan" {
		return validateBackupPlanRequest(r)
	}
	if r.BackupPlan != nil {
		return errors.New("backup plan fields require a plan request")
	}
	if r.Action == "backup-repository" {
		return validateBackupRequest(r)
	}
	if r.Backup != nil {
		return errors.New("backup fields require a repository request")
	}
	if r.Config != nil && r.Config.Release != nil && r.Action == "deploy" {
		if !r.Partial || r.ContextFormat != BundleFormat {
			return errors.New("release commands require a current partial-manifest client")
		}
		if r.Service != "" && r.Service != r.Config.Release.Service {
			return errors.New("release runner is outside the selected service scope; use a manifest without a release command for dependency-only deployment")
		}
	}
	if r.Replica < 0 || r.Replica > config.MaxReplicas || (r.Replica != 0 && (r.Action != "logs" || r.RunOperation != "")) {
		return errors.New("replica ordinal is logs-only and must be between 1 and 8")
	}
	if r.Partial && (r.Action != "deploy" || r.ContextFormat != BundleFormat) {
		return errors.New("partial manifests require deploy with a current build bundle")
	}
	if r.HostIdentity != "" && !operation.MatchString(r.HostIdentity) {
		return errors.New("invalid bound host identity")
	}
	if len(r.Bindings) != 0 && !r.Partial {
		return errors.New("service bindings require partial deployment")
	}
	if len(r.Bindings) != 0 && r.HostIdentity == "" {
		return errors.New("service bindings require the bound host identity")
	}
	if len(r.Bindings) > MaxServices {
		return errors.New("too many service bindings")
	}
	for name, id := range r.Bindings {
		if len(name) > 63 || !label.MatchString(name) || !operation.MatchString(id) {
			return errors.New("invalid service binding")
		}
		if r.Config == nil {
			return errors.New("bindings require service declarations")
		}
		if _, ok := r.Config.Services[name]; !ok || (r.Service != "" && name != r.Service) {
			return errors.New("binding is outside the selected services")
		}
	}
	if r.ReAdd && (!r.Partial || r.Service == "" || !r.Apply) {
		return errors.New("re-add requires deploy --service NAME --yes")
	}
	if r.AllowProtected && r.Action != "service-remove" {
		return errors.New("allow-protected requires service removal")
	}
	if r.Action == "service-remove" && r.Service == "" {
		return errors.New("service removal requires an explicit service")
	}
	if r.AllowDowntime && (r.Action != "rollback" || r.Service == "") {
		return errors.New("allow-downtime requires a scoped rollback")
	}
	if r.ReuseData && (r.Action != "deploy" || r.ContextFormat != BundleFormat) {
		return errors.New("reuse-data requires deploy with a current client")
	}
	if r.ContextFormat != "" && ((r.Action != "deploy" && r.Action != "run") || r.ContextFormat != BundleFormat) {
		return errors.New("unsupported context format or action")
	}
	if r.Service != "" && (len(r.Service) > 63 || !label.MatchString(r.Service) || (r.Action != "status" && r.Action != "logs" && r.Action != "deploy" && r.Action != "run" && r.Action != "rollback" && r.Action != "service-remove")) {
		return errors.New("invalid service selector or action")
	}
	if r.RunOperation != "" && (!operation.MatchString(r.RunOperation) || (r.Action != "status" && r.Action != "logs") || r.Service != "" || r.Candidate) {
		return errors.New("run operation selection requires status or logs without service/candidate selection")
	}
	if r.Action == "run" {
		if r.ContextFormat != BundleFormat || r.Service == "" || len(r.Command) == 0 || len(r.Command) > 256 || r.Command[0] == "" {
			return errors.New("run requires a selected service, build bundle, and command")
		}
		bytes := 0
		for _, arg := range r.Command {
			if len(arg) > 8192 {
				return errors.New("run argument exceeds 8 KiB")
			}
			bytes += len(arg)
			if strings.ContainsRune(arg, '\x00') {
				return errors.New("run command contains NUL")
			}
		}
		if bytes > 32<<10 {
			return errors.New("run command exceeds 32 KiB")
		}
	} else if len(r.Command) != 0 {
		return errors.New("command requires run")
	}
	if r.Action == "deploy" && r.Service != "" && r.ContextFormat != BundleFormat {
		return errors.New("scoped deploy requires a current client")
	}
	if r.Candidate && r.Action != "logs" {
		return errors.New("candidate selection requires logs")
	}
	if (r.SecretName != "" || len(r.SecretNames) != 0) && r.Action != "secrets-stage" && r.Action != "secrets-set" {
		return errors.New("secret names require secrets-stage or secrets-set")
	}
	if r.Supersede != "" && (r.Action != "deploy" || !operation.MatchString(r.Supersede)) {
		return errors.New("supersede requires deploy and valid operation ID")
	}
	if r.Version != 1 {
		return errors.New("unsupported request version")
	}
	if r.Action == "apps" {
		if r.App != "" || r.HostIdentity != "" || r.ReAdd || len(r.Bindings) != 0 {
			return errors.New("apps requires a host-only request")
		}
	} else if len(r.App) > 63 || !label.MatchString(r.App) {
		return errors.New("invalid app name")
	}
	if err := lifecycle.ValidateConfig(r.Host); err != nil {
		return err
	}
	if len(r.App+"."+r.Host.Domain) > 253 {
		return errors.New("generated hostname too long")
	}
	if r.Tail < 0 || r.Tail > 1000 {
		return errors.New("tail must be between 0 and 1000")
	}
	if r.Apply && r.Action != "service-remove" && r.Action != "rollback" && r.Action != "cleanup" && r.Action != "destroy" && !(r.Action == "deploy" && r.Service != "") {
		return errors.New("apply requires maintenance or scoped deployment")
	}
	switch r.Action {
	case "deploy", "run":
		if !operation.MatchString(r.Operation) {
			return errors.New("invalid operation ID")
		}
		if err := ValidateApp(r.Config); err != nil {
			return err
		}
		if r.ContextFormat == "" {
			if err := validateLegacyApp(r.Config); err != nil {
				return err
			}
		}
		for name, s := range r.Config.Services {
			if s.HTTP != nil && len(strings.TrimPrefix(ServiceURL(r.App, name, r.Host.Domain), "https://")) > 253 {
				return errors.New("generated service hostname too long")
			}
		}
		if r.Config.Name != r.App {
			return errors.New("config app mismatch")
		}
		if r.Service != "" {
			if _, exists := r.Config.Services[r.Service]; !exists {
				return errors.New("selected service is not configured")
			}
		}
	case "service-remove", "rollback", "cleanup", "destroy":
		if !operation.MatchString(r.Operation) || r.Config != nil {
			return errors.New("maintenance requires operation ID and no config")
		}
	case "resume":
		if !operation.MatchString(r.Operation) || r.Config != nil {
			return errors.New("resume requires operation ID and captured config")
		}
	case "secrets-stage", "secrets-set":
		if !operation.MatchString(r.Operation) || !validSecretNames(r) || r.Config != nil {
			return errors.New("secret staging requires operation ID, unique valid names, and no config")
		}
	case "apps", "status", "logs", "secrets-list":
		if r.Operation != "" || r.Config != nil {
			return errors.New("read-only request cannot supply operation or config")
		}
	default:
		return errors.New("unknown action")
	}
	if r.Action != "logs" && r.Tail != 0 {
		return errors.New("tail is only valid for logs")
	}
	return nil
}
func (m *Manager) path(p string) string  { return filepath.Join(m.Root, strings.TrimPrefix(p, "/")) }
func (m *Manager) dir(app string) string { return m.path("/var/lib/vamos/apps/" + app) }
func safe(p string) error {
	for {
		s, e := os.Lstat(p)
		if e == nil && s.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink %s", p)
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		q := filepath.Dir(p)
		if q == p {
			return nil
		}
		p = q
	}
}
func atomic(p string, b []byte, mode os.FileMode) error {
	if e := safe(p); e != nil {
		return e
	}
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(p), ".deploy-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if e = f.Chmod(mode); e != nil {
		return e
	}
	if _, e = f.Write(b); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), p); e != nil {
		return e
	}
	// Directory syncing is available on deployment hosts, not Windows clients
	// running the filesystem-backed boundary tests (matching lifecycle/registry).
	if runtime.GOOS == "windows" {
		return nil
	}
	d, e := os.Open(filepath.Dir(p))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (m *Manager) save(j *journal) error {
	if j.Version == 0 {
		j.Version = 1
	}
	j.LastError = diagnostic(j.LastError)
	if isMulti(j) {
		if err := validateMultiJournal(j, j.App); err != nil {
			return err
		}
	}
	// Hydrated records are process-local. The journal contains only the bounded
	// immutable chain reference, preview, and execution stage.
	disk := *j
	if j.Maintenance != nil && j.Maintenance.Manifest != nil {
		w := maintenanceDiskIntent(j.Maintenance)
		disk.Maintenance = &w
	}
	b, e := json.Marshal(&disk)
	if e != nil {
		return e
	}
	if len(b) > 2<<20 {
		return errors.New("journal too large")
	}
	return atomic(filepath.Join(m.dir(j.App), "state.json"), b, 0600)
}
func (m *Manager) load(app string) (*journal, error) {
	if len(app) > 63 || !label.MatchString(app) {
		return nil, errors.New("invalid app name")
	}
	p := filepath.Join(m.dir(app), "state.json")
	if e := safe(p); e != nil {
		return nil, e
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if e != nil {
		return nil, e
	}
	if len(b) > 2<<20 {
		return nil, errors.New("journal too large")
	}
	var probe struct {
		Phase  string
		HostID string
	}
	if e = json.Unmarshal(b, &probe); e != nil {
		return nil, e
	}
	if probe.Phase == "destroyed" {
		t, x := appstate.Decode(strings.NewReader(string(b)), app, probe.HostID)
		if x != nil {
			return nil, x
		}
		return &journal{Version: t.Version, Status: Status{App: t.App, Operation: t.Operation, Phase: t.Phase}, HostID: t.HostID}, nil
	}
	var j journal
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e = d.Decode(&j); e != nil {
		return nil, e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("trailing journal JSON")
	}
	if j.Maintenance != nil && j.Maintenance.Manifest != nil {
		w := j.Maintenance
		if len(w.Containers)+len(w.Images)+len(w.Files) != 0 {
			return nil, errors.New("mixed inline and paged maintenance authority")
		}
		if e = m.hydrateMaintenance(&j, w); e != nil {
			// A missing authority page is corrupt managed state, never an absent
			// app. Do not expose os.ErrNotExist to the unmanaged-app dispatcher.
			return nil, errors.New("invalid maintenance manifest: " + diagnostic(e.Error()))
		}
	}
	if e = validateJournal(&j, app); e != nil {
		return nil, e
	}
	j.LastError = diagnostic(j.LastError)
	return &j, nil
}
func (m *Manager) cmd(c context.Context, n string, a ...string) ([]byte, error) {
	r, e := m.Run(c, n, a, nil)
	if e != nil {
		// Runtime Docker output may contain secret environment values. Build output
		// is handled separately: runtime secrets are never supplied to the builder.
		if n == "docker" {
			if c.Err() != nil {
				return nil, fmt.Errorf("docker command: %w", c.Err())
			}
			if errors.Is(e, context.Canceled) {
				return nil, context.Canceled
			}
			if errors.Is(e, context.DeadlineExceeded) {
				return nil, context.DeadlineExceeded
			}
			return nil, engine.ClassifyRuntimeFailure(r.Stderr)
		}
		return nil, fmt.Errorf("%s: %w: %s", n, e, diagnostic(string(r.Stderr)))
	}
	return r.Stdout, nil
}
func (m *Manager) Execute(ctx context.Context, r Request, tar []byte) (resp Response, err error) {
	resp.Version = 1
	defer func() {
		if err != nil {
			resp.Error = diagnostic(err.Error())
			resp.ErrorCode = ErrorCode(err)
		}
		if r.Action == "cleanup" || r.Action == "destroy" || r.Action == "service-remove" || (resp.Plan != nil && (resp.Plan.Action == "cleanup" || resp.Plan.Action == "destroy" || resp.Plan.Action == "service-remove")) {
			resp, err = boundMaintenanceResponse(resp, err)
		}
	}()
	if err = ValidateRequest(r); err != nil {
		return
	}
	mut := r.Mutating()
	if r.Action == "backup-repository" {
		if len(tar) > backup.MaxPrivateBytes || (!r.Backup.NeedsCredentials() && len(tar) != 0) {
			err = errors.New("invalid private repository input")
			return
		}
	} else if r.Action == "secrets-stage" || r.Action == "secrets-set" {
		if _, err = SecretsInput(r, tar); err != nil {
			return
		}
	} else if len(tar) > 32<<20 || (r.Action != "deploy" && r.Action != "run" && len(tar) > 0) {
		err = errors.New("invalid context archive size or action")
		return
	}
	var ok bool
	if m.MutationLock != nil {
		active, unlock, lockErr := m.MutationLock(ctx, r.App, mut)
		if lockErr != nil {
			err = lockErr
			return
		}
		if unlock != nil {
			defer unlock()
		}
		ok = !active
		if mut && !ok {
			err = errHostBusy
			return
		}
	} else {
		p := m.path(lifecycle.LockPath)
		if err = safe(p); err != nil {
			return
		}
		if err = os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			return
		}
		l := flock.New(p, flock.SetPermissions(0600))
		defer l.Close()
		var e error
		ok, e = l.TryLock()
		if e != nil {
			err = e
			return
		}
		if mut && !ok {
			err = errHostBusy
			return
		}
		if !mut && ok {
			_ = l.Unlock()
		}
	}
	h, e := lifecycle.ReadState(m.path(lifecycle.StatePath))
	if e != nil {
		err = e
		return
	}
	if h == nil || h.Phase != "ready" || h.Config != r.Host || (r.HostIdentity != "" && r.HostIdentity != h.ID) {
		err = errHostIdentity
		return
	}
	if r.Action == "backup-plan" {
		manager := backup.NewManager(m.Root, m.RuntimeDir)
		var runtime backup.PlanRuntime
		if r.BackupPlan.Action != "show" {
			journal, loadErr := m.load(r.App)
			if loadErr != nil || journal == nil || journal.HostID != h.ID {
				return resp, errBackupPlanAppState
			}
			var runtimeErr error
			runtime, runtimeErr = m.backupPlanRuntime(ctx, journal)
			if runtimeErr != nil {
				if errors.Is(runtimeErr, errBackupPlanLegacy) {
					return resp, errBackupPlanLegacy
				}
				return resp, errBackupPlanRuntime
			}
		}
		result, executeErr := manager.ExecutePlan(ctx, h.ID, r.App, *r.BackupPlan, runtime)
		resp.BackupPlan = &result
		return resp, executeErr
	}
	if r.Action == "backup-repository" {
		manager := backup.NewManager(m.Root, m.RuntimeDir)
		if m.RepositoryBackend != nil {
			manager.Backend = m.RepositoryBackend
		}
		result, executeErr := manager.Execute(ctx, h.ID, *r.Backup, tar)
		resp.Backup = &result
		return resp, executeErr
	}
	if r.Action == "apps" {
		resp.Apps, err = m.listApps(ctx, h.ID, h.Config)
		return
	}
	j, e := m.load(r.App)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		err = e
		return
	}
	if j != nil && (j.HostID != h.ID || (j.Phase != "destroyed" && j.URL != "https://"+r.App+"."+h.Config.Domain)) {
		err = errForeignApp
		return
	}
	secretState, e := m.secretStore().Load(r.App, h.ID)
	if e != nil {
		err = e
		return
	}
	volumeState, e := m.volumeStore().Load(r.App, h.ID)
	if e != nil {
		return resp, e
	}
	defer func() { m.enrichIdentityResponse(ctx, r, h.ID, &resp) }()
	defer func() { m.enrichDomainResponse(ctx, r, &resp) }()
	defer func() { m.enrichVolumeResponse(ctx, r, h.ID, &resp) }()
	if !mut {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if j == nil && secretState == nil && volumeState == nil {
			err = errors.New("app is unmanaged")
			return
		}
		if r.RunOperation != "" {
			if !isMulti(j) {
				return resp, errors.New("app has no recorded run")
			}
			return m.readRunResponse(ctx, r, j, secretState, !ok)
		}
		if j != nil && isMulti(j) {
			resp, err = m.readMulti(ctx, r, j, secretState, !ok)
			if err == nil && r.Action == "status" && resp.Status != nil {
				m.enrichRunStatus(ctx, j, resp.Status)
			}
			return
		}
		if r.Service != "" && (r.Service != "web" || j == nil || j.Phase == "destroyed") && !(r.Action == "status" && volumeState != nil && volumeState.Records[r.Service].ID != "") {
			err = errors.New("service has no recorded deployment")
			return
		}
		s := secretOnlyStatus(r.App, r.Host.Domain)
		if j != nil {
			s = maintenanceStatus(j, r.Host.Domain)
		}
		s.Secrets = secretStatuses(secretState, j)
		s.Active = !ok
		if j != nil && j.Current != nil {
			s.Container = j.Current.Container
			v, x := m.container(ctx, j, j.Current)
			s.Running = x == nil && v.State.Running
			if x != nil && j.Phase != "destroying" {
				s.LastError = diagnostic(x.Error())
			}
		}
		resp.Status = &s
		if r.Action == "secrets-list" {
			resp.SecretVersions = secretState.List()
		}
		if r.Action == "logs" {
			var target *release
			if j != nil {
				target = j.Current
				if r.Candidate && j.Phase != "draining" {
					target = j.Candidate
				} else if r.Candidate {
					target = nil
				}
			}
			if target == nil {
				err = errors.New("app has no current release")
				return
			}
			v, inspectErr := m.container(ctx, j, target)
			if inspectErr != nil {
				return resp, inspectErr
			}
			if !sessionContainerID.MatchString(v.ID) {
				return resp, errors.New("inspection has no immutable container ID")
			}
			if r.Replica > 1 {
				return resp, errors.New("selected replica has no recorded container")
			}
			n := r.Tail
			if n == 0 {
				n = 100
			}
			var out process.Result
			resp.LogService = "web"
			resp.LogContainer = target.Container
			resp.LogCandidate = r.Candidate
			out, err = m.Run(ctx, "docker", []string{"logs", "--timestamps", "--tail", fmt.Sprint(n), v.ID}, nil)
			resp.Logs = diagnostic(string(out.Stdout) + string(out.Stderr))
		}
		return
	}
	if j != nil && (j.HostID != h.ID || (j.Phase != "destroyed" && j.URL != "https://"+r.App+"."+h.Config.Domain)) {
		err = errForeignApp
		return
	}
	if err = m.maintenanceFence(r); err != nil {
		return
	}
	if err = m.recoverMaintenancePublication(j, &r); err != nil {
		return
	}
	if r.Action == "run" {
		return m.executeRun(ctx, r, tar, j, h, secretState)
	}
	if isMulti(j) {
		allowHook := ""
		if r.Action == "resume" && j.Operation == r.Operation && j.Phase == "releasing" && j.Hook != nil {
			allowHook = j.Hook.Operation
		}
		if err = m.preflightRuns(ctx, j, allowHook); err != nil {
			return
		}
	}
	if r.Action == "secrets-stage" || r.Action == "secrets-set" {
		return m.executeSecrets(ctx, r, tar, j, h.ID)
	}
	if j == nil && (secretState != nil || volumeState != nil) && (r.Action == "cleanup" || r.Action == "destroy") {
		s := secretOnlyStatus(r.App, r.Host.Domain)
		s.Secrets = secretStatuses(secretState, nil)
		resp.Status = &s
		resp.Plan = &Plan{Action: r.Action, App: r.App}
		if secretState != nil {
			resp.Plan.Retained = append(resp.Plan.Retained, "secret store: retained; no deployment recorded")
		}
		err = m.maintenanceVolumes(ctx, &journal{HostID: h.ID, Status: Status{App: r.App}}, resp.Plan)
		return
	}
	if r.Action == "service-remove" {
		return m.executeServiceRemoval(ctx, r, j)
	}
	if r.Action == "cleanup" || r.Action == "destroy" || (r.Action == "resume" && j != nil && (j.Maintenance != nil || j.Phase == "destroyed" || j.LastMaintenanceOperation == r.Operation)) {
		resp, err = m.executeMaintenance(ctx, r, j)
		if resp.Status != nil {
			resp.Status.Secrets = secretStatuses(secretState, j)
		}
		return
	}
	if r.Action == "rollback" {
		return m.executeRollback(ctx, r, j, secretState)
	}
	// A current client must still be able to correct a failed legacy web
	// deployment. Finish that operation through its original state machine;
	// upgrade only on a later deployment from a ready journal.
	if r.Action == "deploy" && r.ContextFormat == BundleFormat && r.Supersede != "" && j != nil && j.Version == 1 && j.Phase != "ready" && j.Phase != "destroyed" && validateLegacyApp(r.Config) == nil {
		archives, x := buildcontext.ReadBundle(tar, r.Config)
		if x != nil {
			err = x
			return
		}
		tar = archives[buildcontext.BuildKey(*r.Config.Services["web"].Build)]
		r.ContextFormat = ""
	}
	if r.ContextFormat == BundleFormat || (j != nil && isMulti(j)) {
		return m.executeMulti(ctx, r, tar, j, h, secretState)
	}
	if r.Action == "deploy" {
		if len(tar) == 0 {
			err = errors.New("context archive required")
			return
		}
		if j == nil {
			if _, x := os.Lstat(m.dir(r.App)); !errors.Is(x, os.ErrNotExist) {
				err = errors.New("unjournaled app directory; manual recovery required")
				return
			}
		}
		if j != nil && (j.Operation == r.Operation || releaseOperation(j.Current, r.Operation) || releaseOperation(j.Previous, r.Operation)) {
			err = errors.New("operation ID already used")
			return
		}
		refs, e := secretState.Snapshot(r.Config.Services["web"].Secrets)
		if e != nil {
			err = e
			return
		}
		if r.Supersede != "" {
			if j == nil || j.Operation != r.Supersede || (j.Phase != "building" && j.Phase != "starting" && j.Phase != "promoting") || (j.Phase == "promoting" && j.NewCaddy == "") {
				err = errors.New("supersede does not match safe incomplete operation; resume instead")
				return
			}
			if err = m.supersede(ctx, j); err != nil {
				return
			}
		}
		if j != nil && j.Phase != "ready" && j.Phase != "destroyed" && r.Supersede == "" {
			err = errors.New("incomplete operation requires explicit resume")
			return
		}
		var current, previous *release
		if j != nil {
			current = j.Current
			previous = j.Previous
		}
		j = &journal{Status: Status{App: r.App, URL: "https://" + r.App + "." + r.Host.Domain, Phase: "building", Operation: r.Operation}, HostID: h.ID, Config: r.Config, SecretRefs: refs, Current: current, Previous: previous, Archive: r.Operation + ".context.tar"}
		if err = atomic(filepath.Join(m.dir(r.App), j.Archive), tar, 0600); err != nil {
			return
		}
		if err = m.save(j); err != nil {
			return
		}
	} else if j == nil || j.Operation != r.Operation {
		err = errors.New("resume operation does not match")
		return
	}
	defer func() {
		if err != nil {
			j.LastError = diagnostic(err.Error())
			err = errors.Join(err, m.save(j))
		}
		s := maintenanceStatus(j, r.Host.Domain)
		s.Secrets = secretStatuses(secretState, j)
		s.Running = false
		if j.Current != nil {
			s.Container = j.Current.Container
			v, x := m.container(ctx, j, j.Current)
			s.Running = x == nil && v.State.Running
			if x != nil {
				s.LastError = diagnostic(x.Error())
			}
		}
		resp.Status = &s
	}()
	err = m.advance(ctx, j)
	return
}

func releaseOperation(r *release, op string) bool { return r != nil && r.Operation == op }
func collision(a, b *release) bool {
	return a != nil && b != nil && (a.Container == b.Container || a.Operation == b.Operation)
}
func (m *Manager) supersede(c context.Context, j *journal) error {
	if j.Candidate == nil {
		return nil
	}
	if collision(j.Candidate, j.Current) || collision(j.Candidate, j.Previous) {
		return errors.New("candidate aliases retained release")
	}
	if j.NewCaddy != "" {
		if err := m.verifyUnservedPromotion(c, j); err != nil {
			return err
		}
	}
	if err := m.verifyCurrentRoutes(c, j); err != nil {
		return err
	}
	yes, e := m.exists(c, "container", j.Candidate.Container)
	if e != nil {
		return e
	}
	if !yes {
		return m.recordCancelledPromotion(j)
	}
	if _, e = m.cleanupCandidate(c, j, j.Candidate); e != nil {
		return e
	}
	for _, args := range [][]string{{"update", "--restart=no", j.Candidate.Container}, {"stop", "--time", "30", j.Candidate.Container}, {"rm", j.Candidate.Container}} {
		if _, e = m.cmd(c, "docker", args...); e != nil {
			return e
		}
	}
	if j.NewCaddy != "" {
		if yes, e = m.exists(c, "container", j.Candidate.Container); e != nil {
			return e
		}
		if yes {
			return errors.New("candidate removal not proven")
		}
	}
	return m.recordCancelledPromotion(j)
}

const diagnosticLimit = 4096

func diagnostic(s string) string {
	if len(s) > diagnosticLimit {
		return s[len(s)-diagnosticLimit:]
	}
	return s
}
func validateJournal(j *journal, app string) error {
	if isMulti(j) {
		return validateMultiJournal(j, app)
	}
	if j.SecretUpdate != nil || j.Hook != nil || j.Release != nil || len(j.Selected) != 0 || len(j.Removed) != 0 || j.Rollback != nil || j.Scope != "" || j.Run != nil || j.ReuseData || len(j.Volumes) != 0 || len(j.ServiceStates) != 0 || len(j.BuildImages) != 0 || j.BundleSHA != "" || len(j.ServiceOrder) != 0 || j.ActiveService != "" || j.Completed != nil || j.PreviousCompleted != nil {
		return errors.New("invalid legacy app journal")
	}
	bad := errors.New("invalid app journal")
	if j.Phase == "destroyed" {
		if len(j.SecretRefs) != 0 || j.Config != nil || j.Current != nil || j.Previous != nil || j.Candidate != nil || j.Maintenance != nil || j.URL != "" || j.Archive != "" || j.OldCaddy != "" || j.NewCaddy != "" {
			return bad
		}
		return (appstate.Tombstone{Version: j.Version, App: j.App, Operation: j.Operation, Phase: j.Phase, HostID: j.HostID}).Validate(app, j.HostID)
	}
	if err := validateMaintenance(j); err != nil {
		return err
	}
	if j.Version != 1 || len(app) > 63 || !label.MatchString(app) || j.App != app || !operation.MatchString(j.Operation) || !operation.MatchString(j.HostID) || validateLegacyApp(j.Config) != nil || j.Config.Name != app || j.Archive != j.Operation+".context.tar" {
		return bad
	}
	if !matchingSecretRefs(j.SecretRefs, j.Config.Services["web"].Secrets) {
		return bad
	}
	if j.Candidate != nil && !maps.Equal(j.Candidate.SecretRefs, j.SecretRefs) {
		return bad
	}
	if j.Current != nil && j.Current.Operation == j.Operation && !maps.Equal(j.Current.SecretRefs, j.SecretRefs) {
		return bad
	}
	prefix := "https://" + app + "."
	if !strings.HasPrefix(j.URL, prefix) || len(strings.TrimPrefix(j.URL, "https://")) > 253 || lifecycle.ValidateConfig(lifecycle.Config{Name: "host", Domain: strings.TrimPrefix(j.URL, prefix)}) != nil {
		return bad
	}
	switch j.Phase {
	case "building":
	case "starting", "promoting":
		if j.Candidate == nil {
			return bad
		}
	case "ready", "draining", "cleaning", "destroying":
		if j.Current == nil {
			return bad
		}
	default:
		return bad
	}
	if (j.OldCaddy == "") != (j.NewCaddy == "") {
		return bad
	}
	for _, r := range []*release{j.Current, j.Previous, j.Candidate} {
		if r == nil {
			continue
		}
		if len(r.Instances) != 0 || r.ExecutionIntent || r.VolumeID != "" || r.Service != "" || r.Config != nil || !validSecretRefs(r.SecretRefs) || !operation.MatchString(r.Operation) || r.Container != "vamos-"+app+"-"+r.Operation || !imageID.MatchString(r.Image) {
			return bad
		}
		for _, port := range []string{r.Port, r.PublishPort} {
			if port == "" {
				continue
			}
			p, e := strconv.Atoi(port)
			if e != nil || p < 1 || p > 65535 || strconv.Itoa(p) != port {
				return bad
			}
		}
		if r.PublishPort != "" && r.Port != "" && r.PublishPort != r.Port {
			return bad
		}
	}
	if j.Current != nil && j.Current.Port == "" {
		return bad
	}
	if j.Candidate != nil && j.Candidate.Operation != j.Operation {
		return bad
	}
	if j.Container != "" && (j.Current == nil || j.Container != j.Current.Container) {
		return bad
	}
	return nil
}
