package deploy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/process"
)

func TestVolumeWriterExecutionOrdering(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(volumeFlowApp("old"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	old := f.load()
	run := f.m.Run
	f.m.Run = func(ctx context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "docker" && (a[0] == "start" || a[0] == "stop") {
			name := a[len(a)-1]
			v := f.docker.containers[name]
			service := v.Config.Labels["vamos.service"]
			j := f.load()
			slot := j.ServiceStates[service]
			if a[0] == "start" {
				if !slot.Candidate.ExecutionIntent || !slot.DataMayHaveChanged {
					t.Fatal("execution not captured before start")
				}
				for otherName, other := range f.docker.containers {
					if otherName != name && other.Config.Labels["vamos.service"] == service && (other.State.Running || other.HostConfig.RestartPolicy.Name != "no") {
						t.Fatal("second writer authorized")
					}
				}
			}
			if service == "web" {
				routes, err := os.ReadFile(f.m.path("/etc/caddy/Caddyfile"))
				if err != nil {
					t.Fatal(err)
				}
				if !slot.Unavailable || !strings.Contains(string(routes), `respond "Service unavailable" 503`) || !strings.Contains(string(f.live), "respond") {
					t.Fatal("public writer mutated before proven durable503")
				}
				if a[0] == "stop" && name == old.ServiceStates["web"].Current.Container {
					candidate := f.docker.containers[slot.Candidate.Container]
					if candidate == nil || candidate.State.Running || candidate.HostConfig.RestartPolicy.Name != "no" || slot.Candidate.ExecutionIntent {
						t.Fatal("candidate not prepared stopped before retirement")
					}
				}
			}
		}
		return run(ctx, n, a, b)
	}
	if _, err := f.deploy(volumeFlowApp("new"), strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
}

func TestVolumeFailedReadinessResumeAndSupersede(t *testing.T) {
	for _, first := range []bool{false, true} {
		name := "replacement"
		if first {
			name = "first deploy"
		}
		t.Run(name, func(t *testing.T) {
			f := newMultiFlow(t)
			if !first {
				if _, err := f.deploy(volumeFlowApp("old"), strings.Repeat("1", 32)); err != nil {
					t.Fatal(err)
				}
			}
			f.failHealth["web"] = true
			failed := strings.Repeat("2", 32)
			if _, err := f.deploy(volumeFlowApp("broken"), failed); err == nil {
				t.Fatal("readiness failure accepted")
			}
			j := f.load()
			slot := j.ServiceStates["web"]
			id := slot.VolumeID
			candidate := slot.Candidate.Container
			if !slot.Unavailable || !slot.DataMayHaveChanged || !slot.Candidate.ExecutionIntent || j.NewCaddy != "" {
				t.Fatal("lost settled unavailable/data state")
			}
			if slot.Current != nil && (!slot.CurrentRetired || f.docker.containers[slot.Current.Container].State.Running) {
				t.Fatal("old writer resurrected")
			}
			if _, err := f.resume(failed); err == nil {
				t.Fatal("still-broken resume succeeded")
			}
			f.failHealth["web"] = false
			q := f.req
			q.Operation = strings.Repeat("3", 32)
			q.Supersede = failed
			q.Config = volumeFlowApp("fixed")
			if _, err := f.m.Execute(context.Background(), q, f.bundle(q.Config)); err != nil {
				t.Fatal(err)
			}
			after := f.load().ServiceStates["web"]
			if after.VolumeID != id || after.Unavailable || after.CurrentRetired || !after.DataMayHaveChanged || f.docker.containers[candidate] != nil {
				t.Fatal("supersede lost data binding or failed candidate")
			}
		})
	}
}

func TestVolumeWithdrawalFailureCannotRetireWriter(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(volumeFlowApp("old"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	old := f.load().ServiceStates["web"].Current.Container
	run := f.m.Run
	f.m.Run = func(ctx context.Context, n string, a []string, b []byte) (process.Result, error) {
		if ca, ok := caddyCall(f.m, n, a); ok {
			n, a = "caddy", ca
		}
		if n == "caddy" && a[0] == "reload" {
			return process.Result{}, errors.New("lost reload before apply")
		}
		return run(ctx, n, a, b)
	}
	op := strings.Repeat("2", 32)
	if _, err := f.deploy(volumeFlowApp("new"), op); err == nil {
		t.Fatal("uncertain withdrawal accepted")
	}
	slot := f.load().ServiceStates["web"]
	if slot.Phase != "withdrawing" || slot.CurrentRetired || slot.Unavailable || slot.Candidate.ExecutionIntent || !f.docker.containers[old].State.Running {
		t.Fatal("retired before route withdrawal proof")
	}
	q := f.req
	q.Operation = strings.Repeat("3", 32)
	q.Supersede = op
	q.Config = volumeFlowApp("fixed")
	if _, err := f.m.Execute(context.Background(), q, f.bundle(q.Config)); err == nil {
		t.Fatal("superseded uncertain withdrawal")
	}
	f.m.Run = run
	if _, err := f.resume(op); err != nil {
		t.Fatal(err)
	}
}

func TestVolumeBindingChangesRejectedBeforeMutation(t *testing.T) {
	for _, mutate := range []func(*config.App){
		func(a *config.App) { s := a.Services["web"]; s.Volume = nil; a.Services["web"] = s },
		func(a *config.App) { a.Services["web"].Volume.Name = "other" },
		func(a *config.App) { a.Services["web"].Volume.Mount = "/other" },
		func(a *config.App) { delete(a.Services, "web") },
	} {
		f := newMultiFlow(t)
		if _, err := f.deploy(volumeFlowApp("one"), strings.Repeat("1", 32)); err != nil {
			t.Fatal(err)
		}
		a := volumeFlowApp("two")
		mutate(a)
		before := len(f.docker.events)
		if _, err := f.deploy(a, strings.Repeat("2", 32)); err == nil {
			t.Fatal("binding change accepted")
		}
		if len(f.docker.events) != before {
			t.Fatal("mutated Docker before rejecting binding")
		}
	}
}

func TestVolumeSettledUnavailableRouteSurvivesOtherApp(t *testing.T) {
	f := newMultiFlow(t)
	f.failHealth["web"] = true
	if _, err := f.deploy(volumeFlowApp("broken"), strings.Repeat("1", 32)); err == nil {
		t.Fatal("expected readiness failure")
	}
	other := routeTestJournal("neighbor")
	routes, err := f.m.routes(other, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(routes, "notes.example.com {\n\trespond") || !strings.Contains(routes, "neighbor.example.com {\n\tcoraza_waf {\n") {
		t.Fatal("other promotion lost unavailable route", routes)
	}
}
