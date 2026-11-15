package deploy

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
)

func TestReplicaScopedAndManualProtection(t *testing.T) {
	f := newMultiFlow(t)
	f.req.Partial = true
	a := replicaApp(2)
	worker := a.Services["backend"]
	worker.Update = config.UpdateManual
	a.Services["backend"] = worker
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	old := j.ServiceStates["backend"].Current
	f.req.HostIdentity = j.HostID
	f.req.Bindings = map[string]string{"web": j.ServiceStates["web"].Incarnation, "backend": j.ServiceStates["backend"].Incarnation}
	count := 3
	worker.Replicas = &count
	a.Services["backend"] = worker
	before := len(f.docker.events)
	if _, err := f.deploy(a, strings.Repeat("2", 32)); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatal("manual count change accepted", err)
	}
	if len(f.docker.events) != before {
		t.Fatal("protected update mutated runtime")
	}
	// A local pending worker count is outside a scoped web update and is ignored.
	f.req.Service = "web"
	f.req.Bindings = map[string]string{"web": j.ServiceStates["web"].Incarnation}
	web := a.Services["web"]
	web.Replicas = &count
	a.Services["web"] = web
	q := f.req
	q.Config, q.Operation = a, strings.Repeat("3", 32)
	if _, err := f.m.Execute(context.Background(), q, f.bundle(&config.App{Name: a.Name, Services: map[string]config.Service{"web": web}})); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.load().ServiceStates["backend"].Current, old) {
		t.Fatal("unselected group changed")
	}
	// Removing manual protection also needs explicit scoped approval.
	f.req.Service = "backend"
	f.req.Bindings = map[string]string{"backend": j.ServiceStates["backend"].Incarnation}
	f.req.Apply = true
	q = f.req
	q.Config, q.Operation = a, strings.Repeat("4", 32)
	if _, err := f.m.Execute(context.Background(), q, f.bundle(&config.App{Name: a.Name, Services: map[string]config.Service{"backend": worker}})); err != nil {
		t.Fatal(err)
	}
	if len(f.load().ServiceStates["backend"].Current.Instances) != 3 {
		t.Fatal("approved count change lost")
	}
}

func TestReplicaMergedAppLimit(t *testing.T) {
	eight := 8
	old := &journal{Version: 8, ServiceStates: map[string]*serviceState{}}
	for _, name := range []string{"a", "b", "c", "d"} {
		cfg := config.Service{Image: "demo:latest", Update: config.UpdateAuto, Replicas: &eight}
		old.ServiceStates[name] = &serviceState{Current: &release{Config: &cfg}}
	}
	a := &config.App{Name: "notes", Services: map[string]config.Service{"extra": {Image: "demo:latest", Update: config.UpdateAuto}}}
	if _, err := effectiveConfig(Request{App: "notes", Partial: true, Config: a}, old); err == nil {
		t.Fatal("merged count exceeded 32")
	}
}

func TestReplicaExplicitOneAndLegacyRoutesUnchanged(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	old := f.load()
	routes, _ := f.m.routes(old, false)
	f.req.Partial = true
	f.req.HostIdentity = old.HostID
	f.req.Bindings = map[string]string{}
	a := multiApp()
	one := 1
	for name, s := range a.Services {
		s.Replicas = &one
		a.Services[name] = s
		f.req.Bindings[name] = serviceIncarnation(old, name)
	}
	if _, err := f.deploy(a, strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	for name, s := range j.ServiceStates {
		if !reflect.DeepEqual(s.Current, old.ServiceStates[name].Current) {
			t.Fatal("explicit one restarted legacy runtime")
		}
	}
	if next, _ := f.m.routes(j, false); next != routes {
		t.Fatal("singleton route bytes changed")
	}
	q := SessionRequest{Version: 1, Action: "exec", App: j.App, Host: f.req.Host, Service: "web", Replica: 1, Command: []string{"true"}}
	target, err := f.m.ResolveSession(context.Background(), q)
	if err != nil || target.Container != old.ServiceStates["web"].Current.Container {
		t.Fatal("legacy singleton selector failed", err)
	}
}

func TestReplicaScopedRollbackUsesServiceHistoryAndAllCustomUpstreams(t *testing.T) {
	f := newMultiFlow(t)
	f.req.Partial = true
	a := replicaApp(3)
	a.Services["web"].HTTP.Domains = []string{"www.notes.example.net"}
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	initial := f.load()
	f.req.HostIdentity = initial.HostID
	f.req.Bindings = map[string]string{}
	for name, slot := range initial.ServiceStates {
		f.req.Bindings[name] = slot.Incarnation
	}
	one, two := 1, 2
	web, worker := a.Services["web"], a.Services["backend"]
	web.Replicas, worker.Replicas = &one, &two
	a.Services["web"], a.Services["backend"] = web, worker
	if _, err := f.deploy(a, strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	// A subsequent worker-only effective change must not evict web's predecessor.
	worker.Replicas = &one
	a.Services["backend"] = worker
	if _, err := f.deploy(a, strings.Repeat("3", 32)); err != nil {
		t.Fatal(err)
	}
	before := f.load()
	q := Request{Version: 1, Action: "rollback", App: f.req.App, Host: f.req.Host, Service: "web", Operation: strings.Repeat("4", 32), Apply: true}
	if _, err := f.m.Execute(context.Background(), q, nil); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if j.Version != 8 || len(j.ServiceStates["web"].Current.Instances) != 3 {
		t.Fatal("scoped rollback lost group or downgraded")
	}
	if !reflect.DeepEqual(j.ServiceStates["backend"].Current, before.ServiceStates["backend"].Current) {
		t.Fatal("scoped rollback changed excluded group")
	}
	routes, err := f.m.routes(j, false)
	if err != nil {
		t.Fatal(err)
	}
	var upstreams []string
	for _, ref := range runtimeRefs(j.ServiceStates["web"].Current) {
		if ref.Operation != q.Operation || ref.Container != instanceName(j.App, "web", q.Operation, ref.Ordinal) {
			t.Fatal("rollback reused historical identity")
		}
		upstreams = append(upstreams, "127.0.0.1:"+*ref.Port)
	}
	for _, host := range []string{"notes.example.com", "www.notes.example.net"} {
		want := host + " {\n\treverse_proxy " + strings.Join(upstreams, " ") + "\n}"
		if !strings.Contains(routes, want) {
			t.Fatalf("missing full ordered upstream set for %s: %s", host, routes)
		}
	}
}
