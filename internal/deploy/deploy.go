// Package deploy implements recoverable stateless app deployment journals.
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/nchapman/soar/internal/appstate"
	"github.com/nchapman/soar/internal/buildcontext"
	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/process"
	"github.com/nchapman/soar/internal/secrets"
)

type Request struct {
	ContextFormat string           `json:"context_format,omitempty"`
	Service       string           `json:"service,omitempty"`
	Candidate     bool             `json:"candidate,omitempty"`
	SecretName    string           `json:"secret_name,omitempty"`
	Apply         bool             `json:"apply,omitempty"`
	Supersede     string           `json:"supersede,omitempty"`
	Version       int              `json:"version"`
	Action        string           `json:"action"`
	App           string           `json:"app"`
	Operation     string           `json:"operation,omitempty"`
	Config        *config.App      `json:"config,omitempty"`
	Host          lifecycle.Config `json:"host"`
	Tail          int              `json:"tail,omitempty"`
}
type Response struct {
	LogService     string          `json:"log_service,omitempty"`
	LogContainer   string          `json:"log_container,omitempty"`
	LogCandidate   bool            `json:"log_candidate,omitempty"`
	Secret         *secrets.Entry  `json:"secret,omitempty"`
	SecretVersions []secrets.Entry `json:"secret_versions,omitempty"`
	Plan           *Plan           `json:"plan,omitempty"`
	Version        int             `json:"version"`
	Status         *Status         `json:"status,omitempty"`
	Logs           string          `json:"logs,omitempty"`
	Error          string          `json:"error,omitempty"`
}
type SecretStatus struct {
	AppliedVersions map[string]string `json:"applied_versions,omitempty"`
	Name            string            `json:"name"`
	StagedVersion   string            `json:"staged_version"`
	AppliedVersion  string            `json:"applied_version,omitempty"`
	Pending         bool              `json:"pending"`
	Services        []string          `json:"services"`
}

type Status struct {
	Services  map[string]ServiceStatus `json:"services,omitempty"`
	Secrets   []SecretStatus           `json:"secrets,omitempty"`
	App       string                   `json:"app"`
	URL       string                   `json:"url"`
	Phase     string                   `json:"phase"`
	Operation string                   `json:"operation"`
	Active    bool                     `json:"active"`
	Container string                   `json:"container"`
	LastError string                   `json:"last_error"`
	Running   bool                     `json:"running"`
}
type release struct {
	Service     string                 `json:",omitempty"`
	Config      *config.Service        `json:",omitempty"`
	SecretRefs  map[string]secrets.Ref `json:",omitempty"`
	Container   string
	Image       string
	Port        string
	PublishPort string
	Operation   string
}
type journal struct {
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
	// RuntimeDir is the private worker staging directory for transient env files.
	RuntimeDir string
	// AllocatePort selects an advisory loopback reservation. Docker's bind is authoritative.
	AllocatePort   func() (string, error)
	Root           string
	Run            Runner
	HTTP           *http.Client
	HealthTimeout  time.Duration
	PrivateObserve time.Duration
	Drain          time.Duration
}

func allocatePort() (string, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	if err := l.Close(); err != nil {
		return "", err
	}
	return port, nil
}

func NewManager() *Manager {
	return &Manager{Root: "/", Run: func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "docker" && len(a) > 0 && a[0] == "buildx" {
			return process.RunTail(c, n, a, b)
		}
		// Full Docker inspection includes runtime values, which can be much larger
		// than the metadata-only SSH response. Never increase the transport budget.
		if n == "docker" && len(a) > 1 && a[1] == "inspect" {
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
	if err := a.Validate(); err != nil {
		return err
	}
	if len(a.Services) > MaxServices {
		return fmt.Errorf("at most %d services are supported", MaxServices)
	}
	mappings := 0
	for name, s := range a.Services {
		if s.Build == nil || s.Image != "" || s.Volume != nil || s.Update == config.UpdateManual || (s.HTTP != nil && len(s.HTTP.Domains) > 0) {
			return fmt.Errorf("service %s: only Dockerfile-built stateless services without manual updates or custom domains are supported", name)
		}
		if s.HTTP != nil && name != "web" && len(a.Name+"-"+name) > 63 {
			return fmt.Errorf("service %s: generated hostname label exceeds 63 bytes", name)
		}
		mappings += len(s.Secrets)
	}
	if mappings > secrets.MaxNames {
		return errors.New("at most 64 secret environment mappings across the app are supported")
	}
	return nil
}

func validateLegacyApp(a *config.App) error {
	if err := ValidateApp(a); err != nil {
		return err
	}
	s, ok := a.Services["web"]
	if !ok || len(a.Services) != 1 || s.HTTP == nil {
		return errors.New("legacy protocol supports only one public web service; use a current client with a captured build bundle")
	}
	return nil
}
func (r Request) Mutating() bool {
	return r.Action == "deploy" || r.Action == "resume" || r.Action == "cleanup" || r.Action == "destroy" || r.Action == "secrets-stage"
}

func ValidateRequest(r Request) error {
	if r.ContextFormat != "" && (r.Action != "deploy" || r.ContextFormat != BundleFormat) {
		return errors.New("unsupported context format or action")
	}
	if r.Service != "" && (len(r.Service) > 63 || !label.MatchString(r.Service) || (r.Action != "status" && r.Action != "logs")) {
		return errors.New("service selection requires status or logs and a valid service name")
	}
	if r.Candidate && r.Action != "logs" {
		return errors.New("candidate selection requires logs")
	}
	if r.SecretName != "" && r.Action != "secrets-stage" {
		return errors.New("secret name requires secrets-stage")
	}
	if r.Supersede != "" && (r.Action != "deploy" || !operation.MatchString(r.Supersede)) {
		return errors.New("supersede requires deploy and valid operation ID")
	}
	if r.Version != 1 {
		return errors.New("unsupported request version")
	}
	if len(r.App) > 63 || !label.MatchString(r.App) {
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
	if r.Apply && r.Action != "cleanup" && r.Action != "destroy" {
		return errors.New("apply requires maintenance action")
	}
	switch r.Action {
	case "deploy":
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
	case "cleanup", "destroy":
		if !operation.MatchString(r.Operation) || r.Config != nil {
			return errors.New("maintenance requires operation ID and no config")
		}
	case "resume":
		if !operation.MatchString(r.Operation) || r.Config != nil {
			return errors.New("resume requires operation ID and captured config")
		}
	case "secrets-stage":
		if !operation.MatchString(r.Operation) || !secrets.ValidName(r.SecretName) || r.Config != nil {
			return errors.New("secret staging requires operation ID, valid name, and no config")
		}
	case "status", "logs", "secrets-list":
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
func (m *Manager) dir(app string) string { return m.path("/var/lib/soar/apps/" + app) }
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
	if j.Version == 2 {
		if err := validateMultiJournal(j, j.App); err != nil {
			return err
		}
	}
	b, e := json.Marshal(j)
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
			return nil, errors.New("Docker command failed; runtime diagnostic output withheld")
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
		}
	}()
	if err = ValidateRequest(r); err != nil {
		return
	}
	mut := r.Mutating()
	if r.Action == "secrets-stage" {
		if err = secrets.ValidateValue(tar); err != nil {
			return
		}
	} else if len(tar) > 32<<20 || (r.Action != "deploy" && len(tar) > 0) {
		err = errors.New("invalid context archive size or action")
		return
	}
	p := m.path(lifecycle.LockPath)
	if err = safe(p); err != nil {
		return
	}
	if err = os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return
	}
	l := flock.New(p, flock.SetPermissions(0600))
	defer l.Close()
	ok, e := l.TryLock()
	if e != nil {
		err = e
		return
	}
	if mut && !ok {
		err = errors.New("host mutation already active")
		return
	}
	if !mut && ok {
		_ = l.Unlock()
	}
	h, e := lifecycle.ReadState(m.path(lifecycle.StatePath))
	if e != nil {
		err = e
		return
	}
	if h == nil || h.Phase != "ready" || h.Config != r.Host {
		err = errors.New("expected ready host identity does not match")
		return
	}
	j, e := m.load(r.App)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		err = e
		return
	}
	if j != nil && (j.HostID != h.ID || (j.Phase != "destroyed" && j.URL != "https://"+r.App+"."+h.Config.Domain)) {
		err = errors.New("foreign app journal")
		return
	}
	secretState, e := m.secretStore().Load(r.App, h.ID)
	if e != nil {
		err = e
		return
	}
	if !mut {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if j == nil && secretState == nil {
			err = errors.New("app is unmanaged")
			return
		}
		if j != nil && j.Version == 2 {
			return m.readMulti(ctx, r, j, secretState, !ok)
		}
		if r.Service != "" && (r.Service != "web" || j == nil || j.Phase == "destroyed") {
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
			if _, err = m.container(ctx, j, target); err != nil {
				return
			}
			n := r.Tail
			if n == 0 {
				n = 100
			}
			var out process.Result
			resp.LogService = "web"
			resp.LogContainer = target.Container
			resp.LogCandidate = r.Candidate
			out, err = m.Run(ctx, "docker", []string{"logs", "--timestamps", "--tail", fmt.Sprint(n), target.Container}, nil)
			resp.Logs = diagnostic(string(out.Stdout) + string(out.Stderr))
		}
		return
	}
	if j != nil && (j.HostID != h.ID || (j.Phase != "destroyed" && j.URL != "https://"+r.App+"."+h.Config.Domain)) {
		err = errors.New("foreign app journal")
		return
	}
	if err = m.maintenanceFence(r); err != nil {
		return
	}
	if r.Action == "secrets-stage" {
		var entry secrets.Entry
		entry, err = m.secretStore().Stage(r.App, h.ID, r.SecretName, r.Operation, tar)
		if err == nil {
			resp.Secret = &entry
		}
		return
	}
	if j == nil && secretState != nil && (r.Action == "cleanup" || r.Action == "destroy") {
		s := secretOnlyStatus(r.App, r.Host.Domain)
		s.Secrets = secretStatuses(secretState, nil)
		resp.Status = &s
		resp.Plan = &Plan{Action: r.Action, App: r.App, Retained: []string{"secret store: retained; no deployment recorded"}}
		return
	}
	if r.Action == "cleanup" || r.Action == "destroy" || (r.Action == "resume" && j != nil && (j.Maintenance != nil || j.Phase == "destroyed" || j.LastMaintenanceOperation == r.Operation)) {
		resp, err = m.executeMaintenance(ctx, r, j)
		if resp.Status != nil {
			resp.Status.Secrets = secretStatuses(secretState, j)
		}
		return
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
	if r.ContextFormat == BundleFormat || (j != nil && j.Version == 2) {
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
	old, e := m.routes(j, false)
	if e != nil {
		return e
	}
	p := m.path("/etc/caddy/Caddyfile")
	if e = safe(p); e != nil {
		return e
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	if string(b) != old {
		return errors.New("Caddyfile drift")
	}
	expected, e := m.adapt(c, old)
	if e != nil {
		return e
	}
	live, e := m.live(c)
	if e != nil {
		return e
	}
	if !sameJSON(live, expected) {
		return errors.New("Caddy runtime drift")
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
	if j.Version == 2 {
		return validateMultiJournal(j, app)
	}
	if len(j.ServiceStates) != 0 || len(j.BuildImages) != 0 || j.BundleSHA != "" || len(j.ServiceOrder) != 0 || j.ActiveService != "" || j.Completed != nil || j.PreviousCompleted != nil {
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
		if r.Service != "" || r.Config != nil || !validSecretRefs(r.SecretRefs) || !operation.MatchString(r.Operation) || r.Container != "soar-"+app+"-"+r.Operation || !imageID.MatchString(r.Image) {
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
