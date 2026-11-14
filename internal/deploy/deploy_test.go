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
	"strings"
	"testing"
	"time"

	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/process"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func app() *config.App {
	return &config.App{Name: "notes", Services: map[string]config.Service{"web": {Build: &config.Build{Context: ".", Dockerfile: "Dockerfile"}, HTTP: &config.HTTP{Port: 8080, Health: "/ready"}, Update: config.UpdateAuto}}}
}
func setup(t *testing.T) (*Manager, Request) {
	t.Helper()
	m := NewManager()
	m.Root, _ = filepath.EvalSymlinks(t.TempDir())
	m.Builder = fixtureBuilder(m)
	if err := atomic(m.path("/etc/docker/daemon.json"), []byte(`{"bridge":"none","log-driver":"local"}`), 0644); err != nil {
		t.Fatal(err)
	}
	m.Drain = 0
	m.HealthTimeout = time.Millisecond
	h := lifecycle.State{Version: 1, ID: strings.Repeat("a", 32), Config: lifecycle.Config{Name: "host", Domain: "example.com"}, Phase: "ready", Step: "complete", Configured: true, ExecutorSHA: strings.Repeat("b", 64), Packages: []lifecycle.Package{{Name: "docker.io", Version: "1"}}, Accounts: map[string]string{"passwd:caddy": "x", "group:caddy": "x", "group:docker": "x"}}
	b, _ := json.Marshal(h)
	if e := atomic(m.path(lifecycle.StatePath), b, 0600); e != nil {
		t.Fatal(e)
	}
	atomic(m.path("/etc/caddy/Caddyfile"), []byte(baseCaddy), 0644)
	return m, Request{Version: 1, Action: "deploy", App: "notes", Operation: strings.Repeat("c", 32), Config: app(), Host: h.Config}
}
func TestValidation(t *testing.T) {
	_, r := setup(t)
	if e := ValidateRequest(r); e != nil {
		t.Fatal(e)
	}
	for _, mut := range []func(*Request){func(r *Request) { r.App = "../bad" }, func(r *Request) { r.Operation = "ABC" }, func(r *Request) { r.Version = 2 }, func(r *Request) { r.Action = "destroy" }, func(r *Request) { r.Tail = 1001 }, func(r *Request) {
		s := r.Config.Services["web"]
		s.Volume = &config.Volume{Name: "data", Mount: "/data"}
		r.Config.Services["web"] = s
	}} {
		q := r
		q.Config = app()
		mut(&q)
		if ValidateRequest(q) == nil {
			t.Fatalf("accepted %+v", q)
		}
	}
}
func TestBuildFailureCapturesAndResumeUsesArchive(t *testing.T) {
	m, r := setup(t)
	calls := 0
	m.Run = func(_ context.Context, n string, a []string, b []byte) (process.Result, error) {
		calls++
		if n != "docker" || a[0] != "buildx" || string(b) != "saved archive" {
			t.Fatalf("unexpected boundary %s %v %q", n, a, b)
		}
		return process.Result{}, errors.New("build failed")
	}
	resp, e := m.Execute(context.Background(), r, []byte("saved archive"))
	if e == nil || !strings.Contains(resp.Error, "build failed") {
		t.Fatal(resp, e)
	}
	j, e := m.load(r.App)
	if e != nil || j.LastError == "" || j.Phase != "building" {
		t.Fatal(j, e)
	}
	if _, e = m.Execute(context.Background(), r, []byte("different")); e == nil {
		t.Fatal("fresh deploy accepted incomplete operation")
	}
	r.Action = "resume"
	r.Config = nil
	if _, e = m.Execute(context.Background(), r, nil); e == nil {
		t.Fatal("expected build failure")
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	b, _ := os.ReadFile(filepath.Join(m.dir(r.App), j.Archive))
	if string(b) != "saved archive" {
		t.Fatal("archive replaced")
	}
}
func TestPromotionCrashRecoveryAndDrift(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "reconcile", true: "drift"}[drift], func(t *testing.T) {
			m, r := setup(t)
			j := &journal{Status: Status{App: r.App, URL: "https://notes.example.com", Operation: r.Operation, Phase: "promoting"}, Archive: r.Operation + ".context.tar", HostID: strings.Repeat("a", 32), Config: r.Config, Candidate: &release{Container: "soar-" + r.App + "-" + r.Operation, Image: "sha256:" + strings.Repeat("d", 64), Operation: r.Operation, Port: "12345"}}
			m.save(j)
			old, _ := m.routes(j, false)
			next, _ := m.routes(j, true)
			j.OldCaddy = old
			j.NewCaddy = next
			m.save(j)
			atomic(m.path("/etc/caddy/Caddyfile"), []byte(next), 0644)
			live := []byte(`{"text":` + strconvQuote(old) + `}`)
			reloads := 0
			m.Run = func(_ context.Context, n string, a []string, _ []byte) (process.Result, error) {
				if n == "caddy" {
					switch a[0] {
					case "adapt":
						b, _ := os.ReadFile(a[2])
						v, _ := json.Marshal(map[string]string{"text": string(b)})
						return process.Result{Stdout: v}, nil
					case "validate":
						return process.Result{}, nil
					case "reload":
						reloads++
						live = []byte(`{"text":` + strconvQuote(next) + `}`)
						return process.Result{}, nil
					}
				}
				if n == "docker" && a[0] == "container" {
					v := inspection{}
					v.Config.Image = j.Candidate.Image
					v.Config.Labels = map[string]string{"soar.host": j.HostID, "soar.app": j.App, "soar.operation": j.Operation}
					v.State.Running = true
					v.NetworkSettings.Networks = map[string]json.RawMessage{network(j): json.RawMessage(`{}`)}
					b, _ := json.Marshal(v)
					var obj map[string]any
					json.Unmarshal(b, &obj)
					obj["NetworkSettings"].(map[string]any)["Ports"] = map[string]any{"8080/tcp": []any{map[string]string{"HostIP": "127.0.0.1", "HostPort": "12345"}}}
					b, _ = json.Marshal([]any{obj})
					return process.Result{Stdout: b}, nil
				}
				t.Fatalf("unexpected command %s %v", n, a)
				return process.Result{}, nil
			}
			m.HTTP.Transport = transport(func(req *http.Request) (*http.Response, error) {
				b := live
				if req.URL.Port() != "2019" {
					b = []byte("ok")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
			})
			if drift {
				live = []byte(`{"manual":true}`)
			}
			e := m.promote(context.Background(), j)
			if drift {
				if e == nil || reloads != 0 {
					t.Fatal("runtime drift overwritten", e, reloads)
				}
			} else {
				if e != nil || reloads != 1 {
					t.Fatal(e, reloads)
				}
				if e = m.promote(context.Background(), j); e != nil || reloads != 1 {
					t.Fatal("non-idempotent recovery", e, reloads)
				}
			}
		})
	}
}
func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
func TestSymlinkAndHostMismatch(t *testing.T) {
	m, r := setup(t)
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		t.Fatal("runner called")
		return process.Result{}, nil
	}
	r.Host.Name = "other"
	if _, e := m.Execute(context.Background(), r, []byte("tar")); e == nil {
		t.Fatal("host mismatch accepted")
	}
	r.Host.Name = "host"
	os.MkdirAll(filepath.Dir(m.dir(r.App)), 0700)
	os.Symlink(t.TempDir(), m.dir(r.App))
	if _, e := m.Execute(context.Background(), r, []byte("tar")); e == nil {
		t.Fatal("symlink accepted")
	}
}

func TestReadonlyWhileBuildRunning(t *testing.T) {
	m, r := setup(t)
	entered, finish := make(chan struct{}), make(chan struct{})
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		close(entered)
		<-finish
		return process.Result{}, errors.New("failed")
	}
	done := make(chan struct{})
	go func() { defer close(done); m.Execute(context.Background(), r, []byte("archive")) }()
	<-entered
	q := r
	q.Action = "status"
	q.Operation = ""
	q.Config = nil
	resp, e := m.Execute(context.Background(), q, nil)
	if e != nil || resp.Status == nil || !resp.Status.Active || resp.Status.Phase != "building" {
		t.Errorf("status=%+v err=%v", resp, e)
	}
	q.Action = "logs"
	if _, e = m.Execute(context.Background(), q, nil); e == nil {
		t.Error("logs without release accepted")
	}
	close(finish)
	<-done
}

func TestRejectReusedOperationAndUnjournaledDirectory(t *testing.T) {
	for _, prior := range []bool{false, true} {
		t.Run(fmt.Sprint(prior), func(t *testing.T) {
			m, r := setup(t)
			if prior {
				m.save(&journal{Status: Status{App: r.App, URL: "https://notes.example.com", Operation: r.Operation, Phase: "ready"}, Archive: r.Operation + ".context.tar", HostID: strings.Repeat("a", 32), Config: r.Config, Current: &release{Operation: r.Operation, Container: "soar-" + r.App + "-" + r.Operation, Image: "sha256:" + strings.Repeat("d", 64), Port: "12345"}})
			} else {
				os.MkdirAll(m.dir(r.App), 0700)
				os.WriteFile(filepath.Join(m.dir(r.App), "foreign"), []byte("keep"), 0600)
			}
			calls := 0
			m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
				calls++
				return process.Result{}, errors.New("unexpected mutation")
			}
			_, e := m.Execute(context.Background(), r, []byte("archive"))
			if e == nil || calls != 0 {
				t.Fatalf("must refuse before mutation: %v calls=%d", e, calls)
			}
		})
	}
}

func TestSupersedeCapturedInputs(t *testing.T) {
	m, r := setup(t)
	var archives []string
	m.Run = func(_ context.Context, _ string, _ []string, b []byte) (process.Result, error) {
		archives = append(archives, string(b))
		return process.Result{}, errors.New("build failed")
	}
	m.Execute(context.Background(), r, []byte("old"))
	old, _ := m.load(r.App)
	for _, mode := range []string{"wrong", "ready", "promoting"} {
		q := r
		q.Operation = strings.Repeat("d", 32)
		q.Supersede = r.Operation
		j := *old
		if mode == "wrong" {
			q.Supersede = strings.Repeat("e", 32)
		} else {
			j.Phase = mode
		}
		m.save(&j)
		if _, e := m.Execute(context.Background(), q, []byte("new")); e == nil {
			t.Fatal(mode)
		}
		if len(archives) != 1 {
			t.Fatal("refusal built inputs")
		}
	}
	m.save(old)
	q := r
	q.Operation = strings.Repeat("d", 32)
	q.Supersede = r.Operation
	m.Execute(context.Background(), q, []byte("new"))
	j, _ := m.load(r.App)
	if j.Operation != q.Operation || archives[len(archives)-1] != "new" {
		t.Fatal(j, archives)
	}
	b, _ := os.ReadFile(filepath.Join(m.dir(r.App), old.Archive))
	if string(b) != "old" {
		t.Fatal("old archive overwritten")
	}
	q.Action = "resume"
	q.Supersede = ""
	q.Config = nil
	m.Execute(context.Background(), q, nil)
	if archives[len(archives)-1] != "new" {
		t.Fatal(archives)
	}
	q.Action = "status"
	q.Operation = ""
	q.Supersede = r.Operation
	if ValidateRequest(q) == nil {
		t.Fatal("readonly supersede accepted")
	}
}
