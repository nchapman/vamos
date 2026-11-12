package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

// flowEngine models only external boundaries; journals, archives and Caddyfiles
// are real files. Image defaults intentionally survive docker run.
type flowEngine struct {
	t                                       *testing.T
	m                                       *Manager
	containers                              map[string]*inspection
	net                                     *inspection
	image                                   inspection
	live                                    string
	events                                  []string
	archives                                []string
	buildFail, unhealthy, runError, stopped bool
	reloadCrash                             string
	adminFail                               bool
}

func flowSetup(t *testing.T) (*flowEngine, Request) {
	m, r := setup(t)
	f := &flowEngine{t: t, m: m, containers: map[string]*inspection{}, live: baseCaddy}
	f.image.ID = "sha256:" + strings.Repeat("d", 64)
	f.image.Config.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	f.image.Config.Cmd = []string{"/app/server"}
	m.Run = f.run
	m.HTTP.Transport = transport(func(r *http.Request) (*http.Response, error) {
		code := 200
		body := "ok"
		if r.URL.Port() == "2019" {
			if f.adminFail {
				return nil, errors.New("admin interrupted")
			}
			b, _ := json.Marshal(map[string]string{"text": f.live})
			body = string(b)
		} else {
			f.events = append(f.events, "health:"+r.URL.Port())
			if f.unhealthy {
				code = 503
			}
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	return f, r
}
func (f *flowEngine) run(_ context.Context, n string, a []string, b []byte) (process.Result, error) {
	f.t.Helper()
	out := func(v any) (process.Result, error) { b, e := json.Marshal(v); return process.Result{Stdout: b}, e }
	arg := func(k string) string {
		for i := range a {
			if a[i] == k && i+1 < len(a) {
				return a[i+1]
			}
		}
		return ""
	}
	f.events = append(f.events, n+" "+strings.Join(a, " "))
	if n == "caddy" {
		switch a[0] {
		case "adapt":
			b, e := os.ReadFile(arg("--config"))
			if e != nil {
				return process.Result{}, e
			}
			return out(map[string]string{"text": string(b)})
		case "validate":
			return process.Result{}, nil
		case "reload":
			b, e := os.ReadFile(arg("--config"))
			if e != nil {
				return process.Result{}, e
			}
			if f.reloadCrash != "before" {
				f.live = string(b)
			}
			if f.reloadCrash != "" {
				f.adminFail = true
				return process.Result{}, errors.New("reload interrupted")
			}
			return process.Result{}, nil
		}
	}
	if n == "docker" {
		switch a[0] {
		case "buildx":
			f.archives = append(f.archives, string(b))
			if f.buildFail {
				return process.Result{}, errors.New("build failed")
			}
			return process.Result{}, os.WriteFile(arg("--iidfile"), []byte(f.image.ID), 0600)
		case "image":
			return out([]inspection{f.image})
		case "network":
			switch a[1] {
			case "ls":
				if f.net == nil {
					return process.Result{}, nil
				}
				return out(map[string]string{"Name": "soar-notes"})
			case "create":
				f.net = &inspection{Driver: "bridge", Labels: map[string]string{"soar.host": strings.Repeat("a", 32), "soar.app": "notes"}}
				return process.Result{}, nil
			case "inspect":
				return out([]*inspection{f.net})
			}
		case "container":
			if a[1] == "ls" {
				var lines []string
				for k := range f.containers {
					b, _ := json.Marshal(map[string]string{"Names": k})
					lines = append(lines, string(b))
				}
				return process.Result{Stdout: []byte(strings.Join(lines, "\n"))}, nil
			}
			if a[1] == "inspect" {
				v := f.containers[a[2]]
				if v == nil {
					return process.Result{}, errors.New("missing")
				}
				return out([]*inspection{v})
			}
		case "run":
			name := arg("--name")
			if f.containers[name] != nil {
				f.t.Fatal("duplicate run", name)
			}
			v := &inspection{}
			v.ID = name
			v.Config.Image = f.image.ID
			v.Config.Env = append([]string{}, f.image.Config.Env...)
			v.Config.Cmd = append([]string{}, f.image.Config.Cmd...)
			v.Config.Labels = map[string]string{}
			for i, x := range a {
				if x == "--label" {
					k, val, _ := strings.Cut(a[i+1], "=")
					v.Config.Labels[k] = val
				}
				if x == "--env" {
					v.Config.Env = append(v.Config.Env, a[i+1])
				}
				if x == f.image.ID && i+1 < len(a) {
					v.Config.Cmd = append([]string{}, a[i+1:]...)
				}
			}
			v.HostConfig.NetworkMode = arg("--network")
			v.HostConfig.RestartPolicy.Name = arg("--restart")
			v.HostConfig.PortBindings = map[string][]struct{ HostIP, HostPort string }{"8080/tcp": {{HostIP: "127.0.0.1", HostPort: ""}}}
			v.NetworkSettings.Networks = map[string]json.RawMessage{"soar-notes": json.RawMessage(`{}`)}
			v.NetworkSettings.Ports = map[string][]struct{ HostIP, HostPort string }{"8080/tcp": {{HostIP: "127.0.0.1", HostPort: fmt.Sprint(12000 + len(f.containers))}}}
			v.State.Running = !f.stopped
			f.containers[name] = v
			if f.runError {
				return process.Result{}, errors.New("run response lost")
			}
			return process.Result{}, nil
		case "start", "update", "stop", "rm":
			name := a[len(a)-1]
			v := f.containers[name]
			if v == nil {
				f.t.Fatal("mutation of missing container", a)
			}
			switch a[0] {
			case "start":
				v.State.Running = true
			case "update":
				v.HostConfig.RestartPolicy.Name = "no"
			case "stop":
				v.State.Running = false
			case "rm":
				delete(f.containers, name)
			}
			return process.Result{}, nil
		}
	}
	f.t.Fatalf("unexpected boundary %s %v", n, a)
	return process.Result{}, nil
}
func (f *flowEngine) execute(r Request, archive string, wantError bool) *journal {
	f.t.Helper()
	_, e := f.m.Execute(context.Background(), r, []byte(archive))
	if (e != nil) != wantError {
		f.t.Fatalf("Execute %s: %v (want error %v)", r.Action, e, wantError)
	}
	j, e := f.m.load(r.App)
	if e != nil {
		f.t.Fatal(e)
	}
	return j
}
func flowResume(r Request) Request { r.Action = "resume"; r.Config = nil; r.Supersede = ""; return r }
func (f *flowEngine) unchanged(old *release, route string) {
	f.t.Helper()
	j, e := f.m.load("notes")
	if e != nil {
		f.t.Fatal(e)
	}
	b, e := os.ReadFile(f.m.path("/etc/caddy/Caddyfile"))
	if e != nil {
		f.t.Fatal(e)
	}
	if !reflect.DeepEqual(j.Current, old) || f.live != route || string(b) != route || !f.containers[old.Container].State.Running || f.containers[old.Container].HostConfig.RestartPolicy.Name != "unless-stopped" {
		f.t.Fatal("live release changed on failure")
	}
}
func TestFlowSuccessfulReplacement(t *testing.T) {
	f, r := flowSetup(t)
	j := f.execute(r, "first", false)
	if j.Phase != "ready" || j.Current == nil || j.Candidate != nil {
		t.Fatal(j)
	}
	old := j.Current
	r.Operation = strings.Repeat("e", 32)
	f.events = nil
	j = f.execute(r, "second", false)
	if j.Phase != "ready" || j.Previous.Container != old.Container || j.Current.Container == old.Container {
		t.Fatal(j)
	}
	joined := strings.Join(f.events, "\n")
	health := strings.Index(joined, "health:"+j.Current.Port)
	reload := strings.Index(joined, "caddy reload")
	update := strings.Index(joined, "docker update --restart=no "+old.Container)
	stop := strings.Index(joined, "docker stop --time 30 "+old.Container)
	if health < 0 || reload < health || update < reload || stop < update {
		t.Fatal(joined)
	}
	if f.containers[old.Container].State.Running || f.containers[old.Container].HostConfig.RestartPolicy.Name != "no" || !strings.Contains(f.live, j.Current.Port) {
		t.Fatal("replacement not completed")
	}
}
func TestFlowBuildFailurePreservesLive(t *testing.T) {
	f, r := flowSetup(t)
	j := f.execute(r, "first", false)
	route := f.live
	r.Operation = strings.Repeat("e", 32)
	f.buildFail = true
	f.execute(r, "broken", true)
	f.unchanged(j.Current, route)
}
func TestFlowReadinessFailureSupersede(t *testing.T) {
	f, r := flowSetup(t)
	live := f.execute(r, "first", false).Current
	route := f.live
	r.Operation = strings.Repeat("e", 32)
	f.unhealthy = true
	failed := f.execute(r, "broken", true)
	f.unchanged(live, route)
	q := r
	q.Operation = strings.Repeat("f", 32)
	q.Supersede = strings.Repeat("b", 32)
	f.execute(q, "corrected", true)
	f.unchanged(live, route)
	q.Supersede = r.Operation
	q.Config = app()
	s := q.Config.Services["web"]
	s.Env = map[string]string{"FIXED": "yes"}
	s.Command = []string{"/app/server", "--fixed"}
	q.Config.Services["web"] = s
	f.unhealthy = false
	j := f.execute(q, "corrected", false)
	if j.Phase != "ready" || j.Current.Operation != q.Operation || f.containers[failed.Candidate.Container] != nil || f.archives[len(f.archives)-1] != "corrected" {
		t.Fatal(j)
	}
}
func TestFlowRunErrorResume(t *testing.T) {
	for _, stopped := range []bool{true, false} {
		t.Run(fmt.Sprint(stopped), func(t *testing.T) {
			f, r := flowSetup(t)
			f.runError = true
			f.stopped = stopped
			j := f.execute(r, "archive", true)
			name := j.Candidate.Container
			f.runError = false
			f.events = nil
			j = f.execute(flowResume(r), "", false)
			starts := 0
			for _, e := range f.events {
				if strings.HasPrefix(e, "docker run ") || strings.HasPrefix(e, "docker buildx ") {
					t.Fatal("resume recreated candidate", e)
				}
				if e == "docker start "+name {
					starts++
				}
			}
			want := 0
			if stopped {
				want = 1
			}
			if starts != want || j.Current.Container != name || j.Phase != "ready" || len(f.archives) != 1 {
				t.Fatal(j, starts)
			}
		})
	}
}

func TestFlowForeignCandidateNeverMutated(t *testing.T) {
	for _, kind := range []string{"labels", "config"} {
		for _, action := range []string{"resume", "supersede"} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				f, r := flowSetup(t)
				f.runError = true
				f.stopped = true
				j := f.execute(r, "archive", true)
				v := f.containers[j.Candidate.Container]
				if kind == "labels" {
					v.Config.Labels["soar.operation"] = strings.Repeat("b", 32)
				} else {
					v.Config.Cmd = []string{"foreign"}
				}
				f.events = nil
				q := flowResume(r)
				if action == "supersede" {
					q = r
					q.Operation = strings.Repeat("e", 32)
					q.Supersede = r.Operation
				}
				archive := ""
				if action == "supersede" {
					archive = "new"
				}
				f.execute(q, archive, true)
				for _, e := range f.events {
					for _, verb := range []string{"start", "stop", "rm", "update", "run"} {
						if strings.HasPrefix(e, "docker "+verb+" ") {
							t.Fatal("foreign candidate mutated", e)
						}
					}
				}
				if v.State.Running || f.containers[j.Candidate.Container] != v {
					t.Fatal("foreign candidate changed")
				}
			})
		}
	}
}
func TestFlowMissingRecordedCandidateRefusesCreation(t *testing.T) {
	f, r := flowSetup(t)
	f.unhealthy = true
	j := f.execute(r, "archive", true)
	if j.Candidate.Port == "" {
		t.Fatal("endpoint not recorded")
	}
	delete(f.containers, j.Candidate.Container)
	f.events = nil
	f.execute(flowResume(r), "", true)
	for _, e := range f.events {
		if strings.HasPrefix(e, "docker run ") || strings.HasPrefix(e, "docker buildx ") || strings.HasPrefix(e, "docker start ") {
			t.Fatal("missing endpoint recreated", e)
		}
	}
}

// Lose the reload response and admin connection at either side of the runtime
// switch. Execute persists the promotion intent, as it would after interruption.
func TestFlowPromotionCrashRecovery(t *testing.T) {
	for _, boundary := range []string{"before", "after"} {
		t.Run(boundary, func(t *testing.T) {
			f, r := flowSetup(t)
			old := f.execute(r, "first", false).Current
			oldRoute := f.live
			r.Operation = strings.Repeat("e", 32)
			f.reloadCrash = boundary
			pending := f.execute(r, "second", true)
			if pending.Phase != "promoting" || pending.NewCaddy == "" || pending.Current.Container != old.Container {
				t.Fatal(pending)
			}
			b, e := os.ReadFile(f.m.path("/etc/caddy/Caddyfile"))
			if e != nil {
				t.Fatal(e)
			}
			if string(b) != pending.NewCaddy {
				t.Fatal("disk-before-reload intent missing")
			}
			if !f.containers[old.Container].State.Running || f.containers[old.Container].HostConfig.RestartPolicy.Name != "unless-stopped" {
				t.Fatal("old retired before proven switch")
			}
			f.adminFail = false
			f.reloadCrash = ""
			f.events = nil
			if boundary == "before" {
				// Recovery must restore the old durable route even if readiness now fails.
				f.unhealthy = true
				f.execute(flowResume(r), "", true)
				f.unchanged(old, oldRoute)
				f.unhealthy = false
				f.events = nil
			}
			j := f.execute(flowResume(r), "", false)
			reloads := 0
			for _, e := range f.events {
				if strings.HasPrefix(e, "caddy reload ") {
					reloads++
				}
				if strings.HasPrefix(e, "docker run ") || strings.HasPrefix(e, "docker buildx ") {
					t.Fatal("promotion rebuilt", e)
				}
			}
			want := 1
			if boundary == "after" {
				want = 0
			}
			if reloads != want || j.Phase != "ready" || j.Current.Container != pending.Candidate.Container || j.NewCaddy != "" || f.live != pending.NewCaddy {
				t.Fatal(j, reloads)
			}
		})
	}
}
