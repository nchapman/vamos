// Package deploy implements the single-service, stateless remote deployment journal.
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/process"
)

type Request struct {
	Supersede string           `json:"supersede,omitempty"`
	Version   int              `json:"version"`
	Action    string           `json:"action"`
	App       string           `json:"app"`
	Operation string           `json:"operation,omitempty"`
	Config    *config.App      `json:"config,omitempty"`
	Host      lifecycle.Config `json:"host"`
	Tail      int              `json:"tail,omitempty"`
}
type Response struct {
	Version int     `json:"version"`
	Status  *Status `json:"status,omitempty"`
	Logs    string  `json:"logs,omitempty"`
	Error   string  `json:"error,omitempty"`
}
type Status struct {
	App       string `json:"app"`
	URL       string `json:"url"`
	Phase     string `json:"phase"`
	Operation string `json:"operation"`
	Active    bool   `json:"active"`
	Container string `json:"container"`
	LastError string `json:"last_error"`
	Running   bool   `json:"running"`
}
type release struct {
	Container string
	Image     string
	Port      string
	Operation string
}
type journal struct {
	Version int `json:"version"`
	Status
	Archive                      string
	HostID                       string
	Config                       *config.App
	Current, Previous, Candidate *release
	OldCaddy, NewCaddy           string
}
type Runner func(context.Context, string, []string, []byte) (process.Result, error)
type Manager struct {
	Root          string
	Run           Runner
	HTTP          *http.Client
	HealthTimeout time.Duration
	Drain         time.Duration
}

func NewManager() *Manager {
	return &Manager{Root: "/", Run: func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "docker" && len(a) > 0 && a[0] == "buildx" {
			return process.RunTail(c, n, a, b)
		}
		return process.Run(c, n, a, b)
	}, HTTP: &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, HealthTimeout: 30 * time.Second, Drain: 2 * time.Second}
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
	s, ok := a.Services["web"]
	if !ok || len(a.Services) != 1 || s.Build == nil || s.Image != "" || s.HTTP == nil || len(s.HTTP.Domains) > 0 || len(s.Volumes) > 0 || len(s.Secrets) > 0 || s.Update == config.UpdateManual {
		return errors.New("only one build-only public HTTP web service without volumes, secrets, manual updates or custom domains is supported")
	}
	return nil
}
func ValidateRequest(r Request) error {
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
	switch r.Action {
	case "deploy":
		if !operation.MatchString(r.Operation) {
			return errors.New("invalid operation ID")
		}
		if err := ValidateApp(r.Config); err != nil {
			return err
		}
		if r.Config.Name != r.App {
			return errors.New("config app mismatch")
		}
	case "resume":
		if !operation.MatchString(r.Operation) || r.Config != nil {
			return errors.New("resume requires operation ID and captured config")
		}
	case "status", "logs":
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
	j.Version = 1
	j.LastError = diagnostic(j.LastError)
	b, e := json.Marshal(j)
	if e != nil {
		return e
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
	mut := r.Action == "deploy" || r.Action == "resume"
	if len(tar) > 32<<20 || (r.Action != "deploy" && len(tar) > 0) {
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
	if j != nil && (j.HostID != h.ID || j.URL != "https://"+r.App+"."+h.Config.Domain) {
		err = errors.New("foreign app journal")
		return
	}
	if !mut {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if j == nil {
			err = errors.New("app is unmanaged")
			return
		}
		s := j.Status
		s.Active = !ok
		if j.Current != nil {
			s.Container = j.Current.Container
			v, x := m.container(ctx, j, j.Current)
			s.Running = x == nil && v.State.Running
			if x != nil {
				s.LastError = diagnostic(x.Error())
			}
		}
		resp.Status = &s
		if r.Action == "logs" {
			if j.Current == nil {
				err = errors.New("app has no current release")
				return
			}
			if _, err = m.container(ctx, j, j.Current); err != nil {
				return
			}
			n := r.Tail
			if n == 0 {
				n = 100
			}
			var out process.Result
			out, err = m.Run(ctx, "docker", []string{"logs", "--timestamps", "--tail", fmt.Sprint(n), j.Current.Container}, nil)
			resp.Logs = diagnostic(string(out.Stdout) + string(out.Stderr))
		}
		return
	}
	if j != nil && (j.HostID != h.ID || j.URL != "https://"+r.App+"."+h.Config.Domain) {
		err = errors.New("foreign app journal")
		return
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
		if r.Supersede != "" {
			if j == nil || j.Operation != r.Supersede || (j.Phase != "building" && j.Phase != "starting") || j.OldCaddy != "" || j.NewCaddy != "" {
				err = errors.New("supersede does not match safe incomplete operation; resume instead")
				return
			}
			if err = m.supersede(ctx, j); err != nil {
				return
			}
		}
		if j != nil && j.Phase != "ready" && r.Supersede == "" {
			err = errors.New("incomplete operation requires explicit resume")
			return
		}
		var current, previous *release
		if j != nil {
			current = j.Current
			previous = j.Previous
		}
		j = &journal{Status: Status{App: r.App, URL: "https://" + r.App + "." + r.Host.Domain, Phase: "building", Operation: r.Operation}, HostID: h.ID, Config: r.Config, Current: current, Previous: previous, Archive: r.Operation + ".context.tar"}
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
		s := j.Status
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
		return nil
	}
	if _, e = m.cleanupCandidate(c, j, j.Candidate); e != nil {
		return e
	}
	for _, args := range [][]string{{"update", "--restart=no", j.Candidate.Container}, {"stop", "--time", "30", j.Candidate.Container}, {"rm", j.Candidate.Container}} {
		if _, e = m.cmd(c, "docker", args...); e != nil {
			return e
		}
	}
	return nil
}

const diagnosticLimit = 4096

func diagnostic(s string) string {
	if len(s) > diagnosticLimit {
		return s[len(s)-diagnosticLimit:]
	}
	return s
}
func validateJournal(j *journal, app string) error {
	bad := errors.New("invalid app journal")
	if j.Version != 1 || len(app) > 63 || !label.MatchString(app) || j.App != app || !operation.MatchString(j.Operation) || !operation.MatchString(j.HostID) || ValidateApp(j.Config) != nil || j.Config.Name != app || j.Archive != j.Operation+".context.tar" {
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
	case "ready", "draining":
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
		if !operation.MatchString(r.Operation) || r.Container != "soar-"+app+"-"+r.Operation || !imageID.MatchString(r.Image) {
			return bad
		}
		if r.Port != "" {
			p, e := strconv.Atoi(r.Port)
			if e != nil || p < 1 || p > 65535 || strconv.Itoa(p) != r.Port {
				return bad
			}
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
