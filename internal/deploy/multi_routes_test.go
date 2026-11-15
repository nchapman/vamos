package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/vamos/internal/buildcontext"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/process"
)

func routeTestJournal(name string) *journal {
	public := config.Service{Build: &config.Build{Context: ".", Dockerfile: "Dockerfile"}, HTTP: &config.HTTP{Port: 8080, Health: "/ready"}, Update: config.UpdateAuto}
	private := public
	private.HTTP = nil
	j := &journal{Version: 2, Status: Status{App: name, URL: ServiceURL(name, "web", "example.com"), Operation: strings.Repeat("c", 32), Phase: "updating"}, HostID: strings.Repeat("a", 32), Config: &config.App{Name: name, Services: map[string]config.Service{"web": public, "api": public, "worker": private}}, ServiceStates: map[string]*serviceState{}, ActiveService: "api"}
	j.Archive = j.Operation + ".context.tar"
	j.BundleSHA = strings.Repeat("f", 64)
	j.BuildImages = map[string]string{buildcontext.BuildKey(*public.Build): "sha256:" + strings.Repeat("d", 64)}
	j.ServiceOrder = []string{"api", "web", "worker"}
	for name, cfg := range j.Config.Services {
		r := &release{Service: name, Config: &cfg, Operation: strings.Repeat("b", 32), Image: "sha256:" + strings.Repeat("d", 64), Port: "12001"}
		r.Container = containerName(j.App, name, r.Operation)
		if cfg.HTTP == nil {
			r.Port = ""
		}
		r.PublishPort = r.Port
		j.ServiceStates[name] = &serviceState{Phase: "ready", Current: r}
	}
	for _, name := range []string{"api", "web"} {
		next := *j.ServiceStates[name].Current
		next.Operation = j.Operation
		next.Container = containerName(j.App, name, next.Operation)
		next.Port = "12002"
		next.PublishPort = next.Port
		j.ServiceStates[name].Phase = "prepared"
		j.ServiceStates[name].Candidate = &next
	}
	j.ServiceStates["api"].Phase = "promoting"
	return j
}

func saveRouteLegacy(t *testing.T, m *Manager, name, phase string) *journal {
	t.Helper()
	cfg := app()
	cfg.Name = name
	j := &journal{Version: 1, Status: Status{App: name, URL: ServiceURL(name, "web", "example.com"), Operation: strings.Repeat("e", 32), Phase: phase}, HostID: strings.Repeat("a", 32), Config: cfg}
	j.Archive = j.Operation + ".context.tar"
	if phase != "building" {
		j.Current = &release{Container: containerName(name, "", j.Operation), Operation: j.Operation, Image: "sha256:" + strings.Repeat("d", 64), Port: "13001"}
	}
	if e := m.save(j); e != nil {
		t.Fatal(e)
	}
	return j
}

func TestMultiRoutesServiceURL(t *testing.T) {
	for _, tc := range []struct{ service, want string }{{"web", "https://foo.example.com"}, {"api", "https://foo-api.example.com"}, {"bar", "https://foo-bar.example.com"}} {
		if got := ServiceURL("foo", tc.service, "example.com"); got != tc.want {
			t.Fatalf("%s: %s", tc.service, got)
		}
	}
}

func TestMultiRoutesPreviewAndMixedLegacy(t *testing.T) {
	m, _ := setup(t)
	j := routeTestJournal("notes")
	// The first preview must not require an apps directory or a saved own journal.
	before, _ := json.Marshal(j)
	old, e := m.routes(j, false)
	if e != nil {
		t.Fatal(e)
	}
	next, e := m.routes(j, true)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Count(old, "reverse_proxy") != 2 || strings.Contains(old, "worker") {
		t.Fatal(old)
	}
	if !strings.Contains(next, "notes-api.example.com {\n\treverse_proxy 127.0.0.1:12002") || !strings.Contains(next, "notes.example.com {\n\treverse_proxy 127.0.0.1:12001") {
		t.Fatal(next)
	}
	after, _ := json.Marshal(j)
	if string(before) != string(after) {
		t.Fatal("preview mutated journal")
	}
	if e = m.reserveHostnames(t.Context(), j); e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(m.dir(j.App), 0700); e != nil {
		t.Fatal(e)
	}
	if _, e = m.routes(j, false); e != nil {
		t.Fatal("unsaved own directory", e)
	}
	legacy := saveRouteLegacy(t, m, "legacy", "ready")
	next, e = m.routes(j, true)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Count(next, "reverse_proxy") != 3 || !strings.Contains(next, "legacy.example.com {\n\treverse_proxy 127.0.0.1:13001") {
		t.Fatal(next)
	}
	// Maintenance removes every own slot, not just the active service.
	clone := *j
	clone.ServiceStates = nil
	removed, e := m.routes(&clone, true)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Count(removed, "reverse_proxy") != 1 || strings.Contains(removed, "notes") {
		t.Fatal(removed)
	}
	legacy.OldCaddy = old
	legacy.NewCaddy = next
	if e = m.save(legacy); e != nil {
		t.Fatal(e)
	}
	if _, e = m.routes(j, false); e == nil || !strings.Contains(e.Error(), "pending promotion") {
		t.Fatal("missing other-app intent fence", e)
	}
}

func TestMultiRoutesPrivateOnlyIdentity(t *testing.T) {
	m, _ := setup(t)
	j := routeTestJournal("workers")
	delete(j.Config.Services, "web")
	delete(j.Config.Services, "api")
	delete(j.ServiceStates, "web")
	delete(j.ServiceStates, "api")
	j.ActiveService = ""
	text, e := m.routes(j, false)
	if e != nil || text != baseCaddy {
		t.Fatal(text, e)
	}
	if j.URL != "https://workers.example.com" {
		t.Fatal("private-only identity changed")
	}
}

func TestMultiRoutesGlobalClaims(t *testing.T) {
	for _, phase := range []string{"building", "ready"} {
		t.Run(phase, func(t *testing.T) {
			m, _ := setup(t)
			saveRouteLegacy(t, m, "foo-bar", phase)
			j := routeTestJournal("foo")
			cfg := j.Config.Services["api"]
			j.Config.Services["bar"] = cfg
			// No effective bar release is needed: its pending config reserves the name.
			if e := m.reserveHostnames(t.Context(), j); e == nil || !strings.Contains(e.Error(), "foo-bar.example.com") {
				t.Fatal("collision accepted", e)
			}
			if _, e := m.routes(j, false); e == nil {
				t.Fatal("render accepted global collision")
			}
			// A private service has no public claim.
			cfg.HTTP = nil
			j.Config.Services["bar"] = cfg
			if e := m.reserveHostnames(t.Context(), j); e != nil {
				t.Fatal(e)
			}
		})
	}
	// Effective slot config still claims its hostname if desired config differs.
	a := routeTestJournal("foo")
	cfg := a.Config.Services["api"]
	a.ServiceStates["bar"] = &serviceState{Current: &release{Config: &cfg, Port: "13001"}}
	b := routeTestJournal("foo-bar")
	if e := reserveJournalHostnames([]*journal{a, b}, "example.com"); e == nil {
		t.Fatal("effective collision accepted")
	}
	// Same app/service claim in config/current/candidate is not a collision.
	if e := reserveJournalHostnames([]*journal{a, a}, "example.com"); e != nil {
		t.Fatal(e)
	}
}

func TestMultiRoutesForeignAndMalformed(t *testing.T) {
	for _, kind := range []string{"host", "domain", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := setup(t)
			other := saveRouteLegacy(t, m, "other", "building")
			switch kind {
			case "host":
				other.HostID = strings.Repeat("f", 32)
			case "domain":
				other.URL = "https://other.elsewhere.com"
			case "malformed":
				other.URL = "http://other.example.com"
			}
			if e := m.save(other); e != nil {
				t.Fatal(e)
			}
			j := routeTestJournal("notes")
			if e := m.reserveHostnames(t.Context(), j); e == nil {
				t.Fatal("invalid global journal accepted")
			}
			if _, e := m.routes(j, false); e == nil {
				t.Fatal("invalid global journal rendered")
			}
		})
	}
}

func routeTestCaddy(t *testing.T, m *Manager, live *string) {
	t.Helper()
	m.Run = func(_ context.Context, name string, args []string, _ []byte) (process.Result, error) {
		if ca, ok := caddyCall(m, name, args); ok {
			name, args = "caddy", ca
		}
		if name == "caddy" && args[0] == "adapt" {
			b, e := os.ReadFile(args[2])
			if e != nil {
				return process.Result{}, e
			}
			out, _ := json.Marshal(map[string]string{"text": string(b)})
			return process.Result{Stdout: out}, nil
		}
		if name == "caddy" && args[0] == "validate" {
			return process.Result{}, nil
		}
		return process.Result{}, errors.New("runtime deliberately unavailable")
	}
	m.HTTP.Transport = transport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Port() != "2019" {
			return nil, errors.New("unexpected health request")
		}
		b, _ := json.Marshal(map[string]string{"text": *live})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	})
}

func TestMultiRoutesVerifyDrift(t *testing.T) {
	for _, kind := range []string{"none", "disk", "live"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := setup(t)
			j := routeTestJournal("notes")
			expected, e := m.routes(j, false)
			if e != nil {
				t.Fatal(e)
			}
			disk, live := expected, expected
			if kind == "disk" {
				disk = "manual disk"
			}
			if kind == "live" {
				live = "manual live"
			}
			if e = atomic(m.path("/etc/caddy/Caddyfile"), []byte(disk), 0644); e != nil {
				t.Fatal(e)
			}
			routeTestCaddy(t, m, &live)
			e = m.verifyRoutes(context.Background(), j)
			if (e == nil) != (kind == "none") {
				t.Fatal(kind, e)
			}
			b, _ := os.ReadFile(m.path("/etc/caddy/Caddyfile"))
			if string(b) != disk {
				t.Fatal("verification mutated file")
			}
		})
	}
}

func TestMultiRoutesReconcileCrashBoundaries(t *testing.T) {
	for _, diskState := range []string{"old", "new", "drift"} {
		for _, liveState := range []string{"old", "new", "drift"} {
			t.Run(diskState+"/"+liveState, func(t *testing.T) {
				m, _ := setup(t)
				j := routeTestJournal("notes")
				old, e := m.routes(j, false)
				if e != nil {
					t.Fatal(e)
				}
				next, e := m.routes(j, true)
				if e != nil {
					t.Fatal(e)
				}
				j.OldCaddy, j.NewCaddy = old, next
				versions := map[string]string{"old": old, "new": next, "drift": "manual configuration"}
				disk, live := versions[diskState], versions[liveState]
				if e = atomic(m.path("/etc/caddy/Caddyfile"), []byte(disk), 0644); e != nil {
					t.Fatal(e)
				}
				routeTestCaddy(t, m, &live)
				e = m.reconcilePending(context.Background(), j)
				drift := diskState == "drift" || liveState == "drift"
				if (e != nil) != drift {
					t.Fatal(e)
				}
				want := disk
				if !drift && liveState == "old" {
					want = old
				}
				b, _ := os.ReadFile(m.path("/etc/caddy/Caddyfile"))
				if string(b) != want {
					t.Fatal("unexpected reconciliation", string(b))
				}
				if j.Phase != "updating" || j.ServiceStates["api"].Phase != "promoting" || j.NewCaddy != next {
					t.Fatal("reconcile committed parent-owned state")
				}
			})
		}
	}
}

func TestMultiRoutesPromotionIntentAndReadiness(t *testing.T) {
	m, _ := setup(t)
	j := routeTestJournal("notes")
	old, e := m.routes(j, false)
	if e != nil {
		t.Fatal(e)
	}
	next, e := m.routes(j, true)
	if e != nil {
		t.Fatal(e)
	}
	live := old
	routeTestCaddy(t, m, &live)
	if e = atomic(m.path("/etc/caddy/Caddyfile"), []byte(old), 0644); e != nil {
		t.Fatal(e)
	}
	// A failed runtime check leaves durable intent but does not switch routes or
	// perform the parent's slot commit/intent clearing.
	if e = m.promote(context.Background(), j); e == nil {
		t.Fatal("promoted without runtime readiness")
	}
	if j.Phase != "updating" || j.ServiceStates["api"].Phase != "promoting" || j.OldCaddy != old || j.NewCaddy != next {
		t.Fatalf("lost service promotion intent: %+v", j)
	}
	b, _ := os.ReadFile(m.path("/etc/caddy/Caddyfile"))
	if string(b) != old {
		t.Fatal("unhealthy candidate served")
	}
}

func TestMultiRoutesRejectChangedIntent(t *testing.T) {
	m, _ := setup(t)
	j := routeTestJournal("notes")
	old, e := m.routes(j, false)
	if e != nil {
		t.Fatal(e)
	}
	next, e := m.routes(j, true)
	if e != nil {
		t.Fatal(e)
	}
	j.OldCaddy, j.NewCaddy = old, next
	j.ServiceStates["api"].Candidate.Port = "12003"
	for _, fn := range []func(context.Context, *journal) error{m.promote, m.reconcilePending} {
		if e = fn(context.Background(), j); e == nil || !strings.Contains(e.Error(), "intent differs") {
			t.Fatal("changed intent accepted", e)
		}
	}
}

func TestMultiRoutesPersistedV2WithLegacyCaller(t *testing.T) {
	m, _ := setup(t)
	j := routeTestJournal("notes")
	if e := m.save(j); e != nil {
		t.Fatal(e)
	}
	legacy := saveRouteLegacy(t, m, "legacy", "ready")
	text, e := m.routes(legacy, false)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Count(text, "reverse_proxy") != 3 || strings.Contains(text, "12002") || strings.Contains(text, "worker") {
		t.Fatal(text)
	}
	// Pending v2 config reserves names even before it has a slot release.
	pending := routeTestJournal("future")
	pending.Phase = "building"
	pending.ActiveService = ""
	pending.BuildImages = nil
	for _, s := range pending.ServiceStates {
		s.Current = nil
		s.Candidate = nil
		s.Phase = "pending"
		s.New = true
	}
	pending.ServiceOrder = serviceOrder(pending)
	if e = m.save(pending); e != nil {
		t.Fatal(e)
	}
	claimant := routeTestJournal("future-api")
	if e = m.reserveHostnames(t.Context(), claimant); e == nil || !strings.Contains(e.Error(), "future-api.example.com") {
		t.Fatal("pending v2 hostname was not reserved", e)
	}
}

func TestMultiRoutesPromoteResumesOnlyActiveService(t *testing.T) {
	for _, diskNew := range []bool{false, true} {
		for _, liveNew := range []bool{false, true} {
			name := "disk-old"
			if diskNew {
				name = "disk-new"
			}
			if liveNew {
				name += "/live-new"
			} else {
				name += "/live-old"
			}
			t.Run(name, func(t *testing.T) {
				m, _ := setup(t)
				m.HealthTimeout = time.Second
				j := routeTestJournal("notes")
				candidate := j.ServiceStates["api"].Candidate
				cfg := *candidate.Config
				cfg.HTTP = &config.HTTP{Port: 9090, Health: "/api-ready"}
				candidate.Config = &cfg
				j.Config.Services["api"] = cfg
				old, e := m.routes(j, false)
				if e != nil {
					t.Fatal(e)
				}
				next, e := m.routes(j, true)
				if e != nil {
					t.Fatal(e)
				}
				j.OldCaddy, j.NewCaddy = old, next
				disk, live := old, old
				if diskNew {
					disk = next
				}
				if liveNew {
					live = next
				}
				if e = atomic(m.path("/etc/caddy/Caddyfile"), []byte(disk), 0644); e != nil {
					t.Fatal(e)
				}
				reloads, health := 0, 0
				routeTestCaddy(t, m, &live)
				baseRun := m.Run
				m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
					if ca, ok := caddyCall(m, name, args); ok {
						name, args = "caddy", ca
					}
					if name == "caddy" && args[0] == "reload" {
						reloads++
						live = next
						return process.Result{}, nil
					}
					if name != "docker" {
						return baseRun(ctx, name, args, input)
					}
					if len(args) < 3 || args[1] != "inspect" {
						t.Fatalf("unexpected runtime command %v", args)
					}
					var obj any
					switch args[0] {
					case "container":
						if args[2] != candidate.Container {
							t.Fatalf("checked wrong service: %s", args[2])
						}
						ports := map[string]any{"9090/tcp": []map[string]string{{"HostIP": "127.0.0.1", "HostPort": candidate.Port}}}
						obj = map[string]any{
							"Name":            "/" + candidate.Container,
							"Config":          map[string]any{"Image": candidate.Image, "Labels": map[string]string{"vamos.host": j.HostID, "vamos.app": j.App, "vamos.operation": candidate.Operation, "vamos.service": "api"}},
							"State":           map[string]any{"Running": true},
							"HostConfig":      map[string]any{"NetworkMode": network(j), "RestartPolicy": map[string]string{"Name": "unless-stopped"}, "PortBindings": ports},
							"NetworkSettings": map[string]any{"Ports": ports, "Networks": map[string]any{network(j): map[string]any{"Aliases": []string{"api"}}}},
						}
					case "network":
						obj = map[string]any{"Driver": "bridge", "Labels": map[string]string{"vamos.host": j.HostID, "vamos.app": j.App}}
					case "image":
						obj = map[string]any{"Id": candidate.Image}
					default:
						t.Fatalf("unexpected runtime inspection %v", args)
					}
					b, _ := json.Marshal([]any{obj})
					return process.Result{Stdout: b}, nil
				}
				admin := m.HTTP.Transport
				m.HTTP.Transport = transport(func(req *http.Request) (*http.Response, error) {
					if req.URL.Port() == "2019" {
						return admin.RoundTrip(req)
					}
					if req.URL.Port() != candidate.Port || req.URL.Path != "/api-ready" {
						t.Fatalf("wrong readiness URL %s", req.URL)
					}
					health++
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
				})
				if e = m.promote(context.Background(), j); e != nil {
					t.Fatal(e)
				}
				if health != 1 || (!liveNew && reloads != 1) || (liveNew && reloads != 0) {
					t.Fatal("wrong promotion boundaries", health, reloads)
				}
				b, _ := os.ReadFile(m.path("/etc/caddy/Caddyfile"))
				if string(b) != next {
					t.Fatal("candidate file not persisted")
				}
				if j.NewCaddy != next || j.Phase != "updating" || j.ServiceStates["api"].Current.Port != "12001" || j.ServiceStates["web"].Current.Port != "12001" {
					t.Fatal("routing committed parent-owned slot state")
				}
			})
		}
	}
}
