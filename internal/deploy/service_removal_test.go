package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nchapman/vamos/internal/buildcontext"
	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/process"
	"strings"
	"testing"
)

func TestServiceRemovalCommitPreservesOtherHistory(t *testing.T) {
	prior := &release{Operation: strings.Repeat("b", 32)}
	j := &journal{Version: 7, Config: &config.App{Services: map[string]config.Service{"web": {}, "db": {}}}, ServiceStates: map[string]*serviceState{"web": {Incarnation: "web-id"}, "db": {Incarnation: "db-id", Previous: prior}}, Selected: []string{"web", "db"}, ServiceOrder: []string{"web", "db"}, Completed: &completedRelease{Services: map[string]*release{"web": {}, "db": prior}}, PreviousCompleted: &completedRelease{Services: map[string]*release{"web": {}, "db": prior}}, Maintenance: &maintenance{Service: "web", Action: "service-remove", Operation: strings.Repeat("c", 32)}}
	commitServiceRemoval(j)
	if j.Removed["web"] != "web-id" || j.ServiceStates["web"] != nil || j.ServiceStates["db"].Previous != prior {
		t.Fatal("identity or remaining history lost")
	}
	if len(j.Completed.Services) != 1 || len(j.PreviousCompleted.Services) != 1 || len(j.Config.Services) != 1 || len(j.Selected) != 1 || j.Phase != "ready" || j.Maintenance != nil {
		t.Fatal("removed membership retained")
	}
}

func TestServiceRemovalRejectsCrossBoundaryPlan(t *testing.T) {
	j := &journal{Version: 7, ServiceStates: map[string]*serviceState{"web": {}, "db": {}}, Maintenance: &maintenance{Service: "web", OldCaddy: "old", NewCaddy: "new", Containers: []plannedContainer{{Service: "db"}}}}
	if validateServiceRemoval(j) == nil {
		t.Fatal("cross-service container accepted")
	}
	j.Maintenance.Containers = nil
	if err := validateServiceRemoval(j); err != nil {
		t.Fatal(err)
	}
	j.Maintenance.Images = []string{"image"}
	if validateServiceRemoval(j) == nil {
		t.Fatal("shared image deletion accepted")
	}
}

func TestServiceRemovalFailureResumeIsolation(t *testing.T) {
	for _, failure := range []string{"reload", "delete"} {
		t.Run(failure, func(t *testing.T) {
			f := newMultiMaintenanceInventory(t)
			// Large unrelated immutable history must not constrain scoped removal.
			for i := 0; i < 4096; i++ {
				f.file(t, fmt.Sprintf("%032x.%032x.image.iid", i+1, i+1), "sha256:"+strings.Repeat("9", 64)+"\n")
			}
			image := "sha256:" + strings.Repeat("c", 64)
			f.j.Config = &config.App{Name: f.j.App, Services: map[string]config.Service{}}
			f.j.Completed = &completedRelease{Operation: f.j.Operation, Services: map[string]*release{}}
			for i, name := range []string{"api", "web"} {
				r := multiMaintenanceRelease(name, f.j.Operation, image, true)
				if name == "api" {
					r.Config.HTTP.Domains = []string{"claimed.custom.test"}
				}
				r.Port = fmt.Sprint(14001 + i)
				r.PublishPort = r.Port
				f.j.ServiceStates[name] = &serviceState{Phase: "ready", Current: r}
				f.j.Config.Services[name] = *r.Config
				f.j.Completed.Services[name] = r
				f.add(r).State.Running = true
			}
			f.j.ServiceOrder = []string{"api", "web"}
			f.j.BuildImages = map[string]string{buildcontext.BuildKey(*f.j.Config.Services["web"].Build): image}
			f.j.BundleSHA = strings.Repeat("d", 64)
			f.j.Archive = f.j.Operation + ".context.tar"
			f.file(t, f.j.Archive, "bundle")
			upgradeServiceJournal(f.j)
			if err := f.m.save(f.j); err != nil {
				t.Fatal(err)
			}
			untouched, _ := json.Marshal(f.j.ServiceStates["web"])
			caddy, q := flowSetup(t)
			f.m.HTTP = caddy.m.HTTP
			caddy.m = f.m
			docker := f.m.Run
			fail := true
			f.m.Run = func(ctx context.Context, n string, a []string, b []byte) (process.Result, error) {
				if ca, ok := caddyCall(f.m, n, a); ok {
					n, a = "caddy", ca
				}
				if n == "caddy" {
					return caddy.run(ctx, n, a, b)
				}
				if len(a) > 1 && a[0] == "container" && a[1] == "rm" && failure == "delete" && fail {
					return process.Result{}, fmt.Errorf("injected deletion failure")
				}
				return docker(ctx, n, a, b)
			}
			old, err := f.m.routes(f.j, false)
			if err != nil {
				t.Fatal(err)
			}
			caddy.live = old
			if err = atomic(f.m.path("/etc/caddy/Caddyfile"), []byte(old), 0644); err != nil {
				t.Fatal(err)
			}
			q.Action = "service-remove"
			q.Service = "api"
			q.Config = nil
			q.Operation = strings.Repeat("e", 32)
			q.Apply = true
			if failure == "reload" {
				caddy.reloadCrash = "before"
			}
			if _, err = f.m.executeServiceRemoval(context.Background(), q, f.j); err == nil {
				t.Fatal("injected failure succeeded")
			}
			loaded, err := f.m.load(f.j.App)
			if err != nil {
				t.Fatal(err)
			}
			f.j = loaded
			if f.j.Maintenance == nil || f.j.Maintenance.Operation != q.Operation || f.j.ServiceStates["api"] == nil || f.j.Removed["api"] != "" {
				t.Fatal("premature removal commit")
			}
			f.m.DomainProbe = func(context.Context, string) DomainStatus {
				t.Error("removing route must not be probed as configured")
				return DomainStatus{DNS: "matches-host", Certificate: "ready"}
			}
			statusRequest := q
			statusRequest.Action = "status"
			status := maintenanceStatus(f.j, "example.com")
			response := Response{Status: &status}
			f.m.enrichDomainResponse(context.Background(), statusRequest, &response)
			domain := response.Status.Services["api"].Domains[0]
			if domain.Route != "unavailable" || domain.HTTPS != "unknown" {
				t.Fatalf("removing route presented as configured: %+v", domain)
			}
			contender := &journal{Version: 7, Status: Status{App: "other", Phase: "ready"}, Config: &config.App{Name: "other", Services: map[string]config.Service{"web": {HTTP: &config.HTTP{Port: 8080, Domains: []string{"claimed.custom.test"}}}}}}
			if err := reserveJournalHostnames([]*journal{f.j, contender}, "example.com"); err == nil {
				t.Fatal("hostname released before removal commit")
			}
			if failure == "reload" && len(f.mutations) != 0 {
				t.Fatal("resources mutated before route proof")
			}
			before, _ := json.Marshal(f.j.ServiceStates["web"])
			if string(before) != string(untouched) {
				t.Fatal("unselected metadata changed")
			}
			if !strings.Contains(caddy.live, "127.0.0.1:14002") {
				t.Fatal("unselected route lost")
			}
			q.Action = "resume"
			q.Operation = strings.Repeat("f", 32)
			if _, err = f.m.executeServiceRemoval(context.Background(), q, f.j); err == nil {
				t.Fatal("wrong resume admitted")
			}
			q.Operation = strings.Repeat("e", 32)
			fail = false
			caddy.reloadCrash = ""
			caddy.adminFail = false
			if _, err = f.m.executeServiceRemoval(context.Background(), q, f.j); err != nil {
				t.Fatal(err)
			}
			if err := reserveJournalHostnames([]*journal{f.j, contender}, "example.com"); err != nil {
				t.Fatal("hostname retained after commit", err)
			}
			after, _ := json.Marshal(f.j.ServiceStates["web"])
			if string(after) != string(untouched) {
				t.Fatal("unselected metadata changed on commit")
			}
			if len(f.containers) != 1 || len(f.images) != 1 || f.net == nil || f.j.Removed["api"] == "" || f.j.Phase != "ready" {
				t.Fatal("incorrect removal boundary")
			}
			if strings.Contains(caddy.live, "127.0.0.1:14001") || !strings.Contains(caddy.live, "127.0.0.1:14002") {
				t.Fatal("incorrect final routes")
			}
		})
	}
}

func TestServiceRemovalAbandonBoundary(t *testing.T) {
	stable := multiMaintenanceRelease("web", strings.Repeat("b", 32), "sha256:"+strings.Repeat("c", 64), true)
	fresh := func() *journal {
		return &journal{Version: 7, Status: Status{Phase: "updating", Operation: strings.Repeat("d", 32)}, Selected: []string{"broken"}, Config: &config.App{Services: map[string]config.Service{"web": *stable.Config, "broken": *stable.Config}}, ServiceStates: map[string]*serviceState{"web": {Phase: "ready", Current: stable, Skipped: true}, "broken": {Phase: "starting", New: true}}, Completed: &completedRelease{Services: map[string]*release{"web": stable}}}
	}
	if err := validateServiceAbandonment(fresh(), "broken"); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*journal){
		func(j *journal) { j.Selected = []string{"broken", "web"} },
		func(j *journal) { j.NewCaddy = "pending" },
		func(j *journal) { j.ServiceStates["web"].CurrentRetired = true },
		func(j *journal) { j.ServiceStates["web"].Skipped = false },
		func(j *journal) { j.ServiceStates["broken"].New = false },
		func(j *journal) {
			cfg := j.Config.Services["web"]
			cfg.Image = "different"
			j.Config.Services["web"] = cfg
		},
		func(j *journal) { j.Completed = nil },
	} {
		j := fresh()
		mutate(j)
		if err := validateServiceAbandonment(j, "broken"); err == nil || !strings.Contains(err.Error(), "resume") {
			t.Fatal("uncertain boundary accepted", err)
		}
	}
}
