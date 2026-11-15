package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/process"
)

func replicaApp(n int) *config.App {
	a := multiApp()
	for name, s := range a.Services {
		if n != 1 {
			count := n
			s.Replicas = &count
		}
		a.Services[name] = s
	}
	return a
}

func TestReplicaScaleAndRollback(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(replicaApp(1), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	old := f.load()
	f.req.Partial = true
	f.req.HostIdentity = old.HostID
	f.req.Bindings = map[string]string{}
	for name := range old.ServiceStates {
		f.req.Bindings[name] = serviceIncarnation(old, name)
	}
	resp, err := f.deploy(replicaApp(3), strings.Repeat("2", 32))
	if err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if j.Version != 8 {
		t.Fatalf("version %d", j.Version)
	}
	for name, s := range j.ServiceStates {
		if len(s.Current.Instances) != 3 || s.Current.Container != "" {
			t.Fatalf("not a group: %#v", s.Current)
		}
		for i, ref := range runtimeRefs(s.Current) {
			v := f.docker.containers[ref.Container]
			if v == nil || v.Config.Labels["vamos.replica"] != fmt.Sprint(i+1) {
				t.Fatal("missing exact replica label")
			}
			if !retainedContainer(j, ref.Container) {
				t.Fatal("member not retained")
			}
		}
		if f.docker.containers[old.ServiceStates[name].Current.Container].State.Running {
			t.Fatal("old still running")
		}
	}
	routes, err := f.m.routes(j, false)
	if err != nil {
		t.Fatal(err)
	}
	web := j.ServiceStates["web"].Current
	upstreams := []string{}
	for _, r := range runtimeRefs(web) {
		upstreams = append(upstreams, "127.0.0.1:"+*r.Port)
	}
	if !strings.Contains(routes, "reverse_proxy "+strings.Join(upstreams, " ")) {
		t.Fatal(routes)
	}
	if !resp.Status.Services["web"].Running || resp.Status.Services["web"].RunningInstances != 3 {
		t.Fatal(resp.Status)
	}
	q := SessionRequest{Version: 1, Action: "exec", App: j.App, Host: f.req.Host, Service: "web", Command: []string{"true"}}
	if _, err := f.m.ResolveSession(context.Background(), q); err == nil {
		t.Fatal("ambiguous session accepted")
	}
	q.Replica = 2
	target, err := f.m.ResolveSession(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if target.Container != web.Instances[1].Container || target.Replica != 2 {
		t.Fatal(target)
	}
	if _, err := f.deploy(replicaApp(1), strings.Repeat("3", 32)); err != nil {
		t.Fatal(err)
	}
	if len(f.load().ServiceStates["web"].Current.Instances) != 1 {
		t.Fatal("v8 downgraded or scale down failed")
	}
	q2 := f.req
	q2.Partial = false
	q2.Bindings = nil
	q2.HostIdentity = ""
	q2.Action = "rollback"
	q2.Config = nil
	q2.ContextFormat = ""
	q2.Operation = strings.Repeat("4", 32)
	q2.Apply = true
	if _, err := f.m.Execute(context.Background(), q2, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.load().ServiceStates["web"].Current.Instances) != 3 {
		t.Fatal("rollback lost count")
	}
}

func TestReplicaPartialCreateResume(t *testing.T) {
	f := newMultiFlow(t)
	f.req.Partial = true
	run := f.m.Run
	failed := false
	f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "docker" && a[0] == "run" && strings.Contains(strings.Join(a, " "), "vamos.replica=2") && !failed {
			failed = true
			return process.Result{}, errors.New("interrupted create")
		}
		return run(c, n, a, b)
	}
	op := strings.Repeat("1", 32)
	if _, err := f.deploy(replicaApp(3), op); err == nil {
		t.Fatal("expected interruption")
	}
	f.req.Partial = false
	if _, err := f.resume(op); err != nil {
		t.Fatal(err)
	}
	if len(f.docker.containers) != 6 {
		t.Fatal("partial group not recovered")
	}
}
