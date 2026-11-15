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

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/process"
)

// Fail only the final promotion check, after startup and alias admission have
// succeeded. The old endpoint stays healthy and Caddy never reloads.
func failedFinalPromotion(t *testing.T, legacy bool) (*multiFlow, *journal) {
	t.Helper()
	f := newMultiFlow(t)
	deploy := func(a *config.App, op string) error {
		q := f.req
		q.Config = a
		q.Operation = op
		data := f.bundle(a)
		if legacy {
			q.ContextFormat = ""
			data = []byte("captured:Dockerfile")
		}
		_, err := f.m.Execute(context.Background(), q, data)
		return err
	}
	a := multiApp()
	if legacy {
		a = app()
	}
	if err := deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	run := f.m.Run
	unhealthyPort := ""
	transportBefore := f.m.HTTP.Transport
	f.m.HTTP.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if unhealthyPort != "" && r.URL.Port() == unhealthyPort {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unhealthy candidate"))}, nil
		}
		return transportBefore.RoundTrip(r)
	})
	f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
		if ca, ok := caddyCall(f.m, name, args); ok {
			name, args = "caddy", ca
		}
		if name == "caddy" && args[0] == "validate" {
			j := f.load()
			candidate := j.Candidate
			if j.Version == 2 {
				candidate = j.ServiceStates[j.ActiveService].Candidate
			}
			unhealthyPort = candidate.Port
		}
		return run(ctx, name, args, input)
	}
	for name, s := range a.Services {
		s.Env = map[string]string{"REVISION": "broken"}
		a.Services[name] = s
	}
	if err := deploy(a, strings.Repeat("2", 32)); err == nil {
		t.Fatal("expected final readiness failure")
	}
	f.m.Run = run
	j := f.load()
	if j.NewCaddy == "" {
		t.Fatal("missing pending promotion")
	}
	disk, err := os.ReadFile(f.m.path("/etc/caddy/Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(map[string]string{"text": j.OldCaddy})
	if string(disk) != j.OldCaddy || !sameJSON(f.live, expected) {
		t.Fatal("failed promotion changed old routing")
	}
	if _, err = f.resume(j.Operation); err == nil {
		t.Fatal("captured bad candidate unexpectedly recovered")
	}
	return f, f.load()
}

func correctedSupersede(f *multiFlow, legacy bool) error {
	q := f.req
	q.Operation = strings.Repeat("3", 32)
	q.Supersede = strings.Repeat("2", 32)
	q.Config = changedMultiApp("corrected")
	if legacy {
		delete(q.Config.Services, "backend")
	}
	_, err := f.m.Execute(context.Background(), q, f.bundle(q.Config))
	return err
}

func TestSupersedeCancelsProvenUnservedPromotion(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "multi"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			f, j := failedFinalPromotion(t, legacy)
			candidate := j.Candidate
			if !legacy {
				candidate = j.ServiceStates["web"].Candidate
			}
			if err := correctedSupersede(f, legacy); err != nil {
				t.Fatal("corrected supersede cannot settle unserved promotion:", err)
			}
			if f.docker.containers[candidate.Container] != nil {
				t.Fatal("cancelled candidate/alias remains")
			}
			ready := f.load()
			if ready.Phase != "ready" || ready.NewCaddy != "" || ready.Operation != strings.Repeat("3", 32) {
				t.Fatal("corrected deployment not ready")
			}
			if !legacy && ready.PreviousCompleted.Operation != strings.Repeat("1", 32) {
				t.Fatal("failed operation replaced completed history")
			}

			// A second app uses the same durable host state and Caddy configuration.
			other := newMultiFlow(t)
			other.m.Root = f.m.Root
			other.live = f.live
			other.req.App = "other"
			other.docker.j.App = "other"
			other.docker.net.Labels["vamos.app"] = "other"
			a := app()
			a.Name = "other"
			if _, err := other.deploy(a, strings.Repeat("4", 32)); err != nil {
				t.Fatal("unrelated deployment remains fenced:", err)
			}
			if !strings.Contains(string(other.live), "notes.example.com") || !strings.Contains(string(other.live), "other.example.com") {
				t.Fatal("unrelated deployment lost an existing route")
			}
		})
	}
}

func TestCancelledPromotionCanResumeCapturedInputsAfterSupersedeInterruption(t *testing.T) {
	f, j := failedFinalPromotion(t, false)
	candidate := *j.ServiceStates["web"].Candidate
	// Simulate interruption after cancellation, before installing new inputs.
	if err := f.m.supersedeMulti(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	saved := f.load()
	slot := saved.ServiceStates["web"]
	if saved.NewCaddy != "" || slot.Phase != "starting" || slot.Candidate.Port != "" || slot.Candidate.PublishPort != candidate.Port {
		t.Fatal("cancellation did not preserve recreatable captured inputs")
	}
	if _, err := f.resume(saved.Operation); err == nil {
		t.Fatal("original captured bad code unexpectedly healthy")
	}
	recreated := f.docker.containers[candidate.Container]
	if recreated == nil || hasAlias(recreated, network(saved), "web") {
		t.Fatal("original candidate not recreated without alias")
	}
	if f.load().NewCaddy != "" {
		t.Fatal("failed initial readiness reintroduced a promotion fence")
	}
	if err := correctedSupersede(f, false); err != nil {
		t.Fatal(err)
	}
}

func TestPromotionCancellationRejectsEquivalentRoutes(t *testing.T) {
	f, j := failedFinalPromotion(t, false)
	j.ServiceStates["web"].Current.Port = j.ServiceStates["web"].Candidate.Port
	j.OldCaddy = j.NewCaddy
	if err := os.WriteFile(f.m.path("/etc/caddy/Caddyfile"), []byte(j.OldCaddy), 0644); err != nil {
		t.Fatal(err)
	}
	f.live, _ = json.Marshal(map[string]string{"text": j.OldCaddy})
	if err := f.m.verifyUnservedPromotion(context.Background(), j); err == nil || !strings.Contains(err.Error(), "not proven unserved") {
		t.Fatal("equivalent old/new routes are not proof of an unserved candidate", err)
	}
}

func TestPromotionCancellationRejectsUncertainRouting(t *testing.T) {
	for _, mode := range []string{"live-new", "disk-new", "live-drift", "disk-drift", "intent-drift", "admin-unavailable"} {
		t.Run(mode, func(t *testing.T) {
			f, j := failedFinalPromotion(t, false)
			switch mode {
			case "live-new":
				f.live, _ = json.Marshal(map[string]string{"text": j.NewCaddy})
			case "disk-new":
				if err := os.WriteFile(f.m.path("/etc/caddy/Caddyfile"), []byte(j.NewCaddy), 0644); err != nil {
					t.Fatal(err)
				}
			case "live-drift":
				f.live = []byte(`{"unrelated":true}`)
			case "disk-drift":
				if err := os.WriteFile(f.m.path("/etc/caddy/Caddyfile"), []byte("foreign"), 0644); err != nil {
					t.Fatal(err)
				}
			case "intent-drift":
				j.NewCaddy += "\n# foreign intent\n"
				if err := f.m.save(j); err != nil {
					t.Fatal(err)
				}
			case "admin-unavailable":
				f.m.HTTP.Transport = transport(func(*http.Request) (*http.Response, error) { return nil, errors.New("admin unavailable") })
			}
			before := len(f.docker.events)
			if err := correctedSupersede(f, false); err == nil {
				t.Fatal("uncertain promotion cancelled")
			}
			if len(f.docker.events) != before || f.load().NewCaddy == "" {
				t.Fatal("uncertain cancellation mutated runtime or cleared intent")
			}
		})
	}
}

func TestPromotionCancellationRecoversLostRemovalResponses(t *testing.T) {
	for _, step := range []string{"update", "stop", "rm"} {
		t.Run(step, func(t *testing.T) {
			f, j := failedFinalPromotion(t, false)
			candidate := j.ServiceStates["web"].Candidate.Container
			f.docker.lose[step] = 1
			if err := correctedSupersede(f, false); err == nil {
				t.Fatal("expected lost mutation response")
			}
			if f.load().NewCaddy == "" {
				t.Fatal("unproven cancellation cleared intent")
			}
			if err := correctedSupersede(f, false); err != nil {
				t.Fatal("retry did not finish cancellation:", err)
			}
			if f.docker.containers[candidate] != nil || f.load().Phase != "ready" {
				t.Fatal("candidate survived corrected deployment")
			}
		})
	}
}
