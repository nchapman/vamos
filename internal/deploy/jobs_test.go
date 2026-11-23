package deploy

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/vamos/internal/config"
)

func TestScheduledJobsFollowOnlyCurrentServiceRelease(t *testing.T) {
	ctx := context.Background()
	f := newMultiFlow(t)
	f.req.Partial = true
	a := multiApp()
	s := a.Services["backend"]
	replicas := 2
	s.Replicas = &replicas
	cmd := []string{"collect", "two words $HOME"}
	s.Jobs = map[string]config.Job{"collect": {Command: cmd, Schedule: config.Schedule{Every: "15m"}}}
	a.Services["backend"] = s
	op := strings.Repeat("1", 32)
	if _, err := f.deploy(a, op); err != nil {
		t.Fatal(err)
	}
	minute := time.Date(2026, 6, 1, 10, 30, 0, 0, time.Local)
	dueAt := func(at time.Time) []ScheduledJob {
		t.Helper()
		jobs, err := f.m.DueJobs(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		return jobs
	}
	if jobs := dueAt(minute.Add(time.Minute)); len(jobs) != 0 {
		t.Fatal(jobs)
	}
	jobs := dueAt(minute)
	if len(jobs) != 1 || jobs[0].Release != op || jobs[0].Name != "collect" {
		t.Fatal(jobs)
	}
	job := jobs[0]
	args, err := f.m.ScheduledCommand(ctx, job, minute)
	if err != nil {
		t.Fatal(err)
	}
	r := f.load().ServiceStates["backend"].Current
	ref, err := selectRuntime(r, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args, append([]string{"exec", f.docker.containers[ref.Container].ID}, cmd...)) {
		t.Fatal(args)
	}
	q := Request{Action: "status", Version: 1, App: a.Name, Host: f.req.Host, IncludeJobs: true}
	resp, err := f.m.Execute(ctx, q, nil)
	if err != nil || len(resp.Status.Jobs) != 1 || resp.Status.Jobs[0].Next == "" {
		t.Fatal(resp, err)
	}
	q.Service = "web"
	resp, err = f.m.Execute(ctx, q, nil)
	if err != nil || len(resp.Status.Jobs) != 0 {
		t.Fatal(resp, err)
	}
	deployOnly := func(name string, svc config.Service, digit string) {
		t.Helper()
		j := f.load()
		f.req.Bindings = map[string]string{name: j.ServiceStates[name].Incarnation}
		f.req.HostIdentity = j.HostID
		if _, err := f.deploy(&config.App{Name: a.Name, Services: map[string]config.Service{name: svc}}, strings.Repeat(digit, 32)); err != nil {
			t.Fatal(err)
		}
	}
	// An independently deployed service can declare the same job name.
	web := a.Services["web"]
	web.Jobs = map[string]config.Job{"collect": {Command: []string{"web-collect"}, Schedule: config.Schedule{Every: "15m"}}}
	deployOnly("web", web, "2")
	both := dueAt(minute)
	if len(both) != 2 || !slices.Contains(both, job) || both[0].Unit() == both[1].Unit() {
		t.Fatalf("expected independent units for both services: %+v", both)
	}
	s.Jobs = nil
	deployOnly("backend", s, "3")
	if got := dueAt(minute); len(got) != 1 || got[0].Service != "web" || got[0].Name != "collect" {
		t.Fatal(got)
	}
	if _, err := f.m.ScheduledCommand(ctx, job, minute); err == nil {
		t.Fatal("admitted retired schedule")
	}
}
