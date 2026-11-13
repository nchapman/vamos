package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/process"
)

func replicaFlow(t *testing.T) *multiFlow {
	f := newMultiFlow(t)
	f.req.Partial = true
	if _, err := f.deploy(replicaApp(3), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	f.req.HostIdentity = j.HostID
	f.req.Bindings = map[string]string{}
	for name, s := range j.ServiceStates {
		f.req.Bindings[name] = s.Incarnation
	}
	return f
}
func resumeReplicas(f *multiFlow, op string) error {
	q := Request{Version: 1, Action: "resume", App: f.req.App, Host: f.req.Host, Operation: op}
	_, err := f.m.Execute(context.Background(), q, nil)
	return err
}
func replicaChange() *config.App {
	a := replicaApp(3)
	for name, s := range a.Services {
		s.Env = map[string]string{"REV": "changed"}
		a.Services[name] = s
	}
	return a
}

func TestReplicaPublicPortIntentAndLostCreate(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			f := newMultiFlow(t)
			f.req.Partial = true
			a := replicaApp(3)
			delete(a.Services, "backend")
			run := f.m.Run
			failed := false
			capturedPort := ""
			f.m.Run = func(c context.Context, n string, args []string, b []byte) (process.Result, error) {
				if n == "docker" && args[0] == "run" && strings.Contains(strings.Join(args, " "), "soar.replica=2") && !failed {
					member := f.load().ServiceStates["web"].Candidate.Instances[1]
					capturedPort = member.PublishPort
					if capturedPort == "" || !strings.Contains(strings.Join(args, " "), "127.0.0.1:"+capturedPort+":") {
						t.Fatal("port intent not saved before create")
					}
					failed = true
					if lost {
						if _, err := run(c, n, args, b); err != nil {
							return process.Result{}, err
						}
					}
					return process.Result{}, errors.New("create interrupted")
				}
				return run(c, n, args, b)
			}
			op := strings.Repeat("1", 32)
			if _, err := f.deploy(a, op); err == nil || !failed {
				t.Fatal("expected creation failure", err)
			}
			if err := resumeReplicas(f, op); err != nil {
				t.Fatal(err)
			}
			current := f.load().ServiceStates["web"].Current
			if len(f.docker.containers) != 3 || current.Instances[1].Port != capturedPort {
				t.Fatal("resume lost member or port identity")
			}
		})
	}
}

func TestReplicaLostPromotionResponse(t *testing.T) {
	f := replicaFlow(t)
	run := f.m.Run
	failed := false
	adminUnavailable := false
	beforeHTTP := f.m.HTTP.Transport
	f.m.HTTP.Transport = transport(func(req *http.Request) (*http.Response, error) {
		if adminUnavailable && req.URL.Port() == "2019" {
			return nil, errors.New("admin unavailable after lost reload")
		}
		return beforeHTTP.RoundTrip(req)
	})
	f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
		result, err := run(c, n, a, b)
		if n == "caddy" && a[0] == "reload" && !failed && err == nil {
			failed = true
			adminUnavailable = true
			return result, errors.New("lost reload response")
		}
		return result, err
	}
	op := strings.Repeat("2", 32)
	if _, err := f.deploy(replicaChange(), op); err == nil || !failed {
		t.Fatal("expected lost reload", err)
	}
	adminUnavailable = false
	if err := resumeReplicas(f, op); err != nil {
		t.Fatal(err)
	}
	if j := f.load(); j.Phase != "ready" || j.NewCaddy != "" || len(j.ServiceStates["web"].Current.Instances) != 3 {
		t.Fatal("group promotion not reconciled")
	}
}

func TestReplicaInterruptedAliasAndRetirement(t *testing.T) {
	for _, step := range []string{"alias", "public-retire", "private-retire"} {
		t.Run(step, func(t *testing.T) {
			f := replicaFlow(t)
			old := f.load()
			run := f.m.Run
			failed := false
			op := strings.Repeat("2", 32)
			f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
				member := a[len(a)-1]
				fail := n == "docker" && ((step == "alias" && len(a) > 2 && a[0] == "network" && a[1] == "disconnect" && member == instanceName(old.App, "web", op, 2)) || (step == "public-retire" && a[0] == "stop" && member == old.ServiceStates["web"].Current.Instances[1].Container) || (step == "private-retire" && a[0] == "stop" && member == old.ServiceStates["backend"].Current.Instances[1].Container))
				result, err := run(c, n, a, b)
				if fail && !failed && err == nil {
					failed = true
					return result, errors.New("lost response")
				}
				return result, err
			}
			if _, err := f.deploy(replicaChange(), op); err == nil || !failed {
				t.Fatal("expected interrupted group", err)
			}
			if step == "private-retire" {
				for _, v := range f.docker.containers {
					if v.Config.Labels["soar.operation"] == op && v.Config.Labels["soar.service"] == "backend" {
						t.Fatal("new worker created before all old stopped")
					}
				}
			}
			if err := resumeReplicas(f, op); err != nil {
				t.Fatal(err)
			}
			for _, s := range old.ServiceStates {
				for _, r := range runtimeRefs(s.Current) {
					v := f.docker.containers[r.Container]
					if v.State.Running || v.HostConfig.RestartPolicy.Name != "no" {
						t.Fatal("old member survived retirement")
					}
				}
			}
		})
	}
}

func TestReplicaFinalReadinessAndCancellation(t *testing.T) {
	f := replicaFlow(t)
	old := f.load()
	oldRoutes, _ := f.m.routes(old, false)
	run := f.m.Run
	originalHTTP := f.m.HTTP.Transport
	badPort := ""
	f.m.HTTP.Transport = transport(func(req *http.Request) (*http.Response, error) {
		if badPort != "" && req.URL.Port() == badPort {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unhealthy"))}, nil
		}
		return originalHTTP.RoundTrip(req)
	})
	f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "caddy" && a[0] == "validate" {
			badPort = f.load().ServiceStates["web"].Candidate.Instances[1].Port
		}
		return run(c, n, a, b)
	}
	op := strings.Repeat("2", 32)
	if _, err := f.deploy(replicaChange(), op); err == nil {
		t.Fatal("unhealthy member promoted")
	}
	j := f.load()
	if j.NewCaddy == "" || j.OldCaddy != oldRoutes {
		t.Fatal("missing old route intent")
	}
	if routes, _ := f.m.routes(j, false); routes != oldRoutes {
		t.Fatal("old group routes changed")
	}
	for _, ref := range runtimeRefs(old.ServiceStates["web"].Current) {
		if !f.docker.containers[ref.Container].State.Running {
			t.Fatal("old member stopped")
		}
	}
	candidates := j.ServiceStates["web"].Candidate
	// Removal of the middle member succeeds but its response is lost. Intent must
	// survive, and retry must also remove the first/last members idempotently.
	f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
		result, err := run(c, n, a, b)
		if n == "docker" && a[0] == "rm" && a[len(a)-1] == candidates.Instances[1].Container && err == nil {
			return result, errors.New("lost removal response")
		}
		return result, err
	}
	if err := f.m.supersedeMulti(context.Background(), j); err == nil {
		t.Fatal("expected partial cancellation")
	}
	j = f.load()
	if j.NewCaddy == "" {
		t.Fatal("cleared incomplete cancellation")
	}
	f.m.Run = run
	if err := f.m.supersedeMulti(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j = f.load()
	for _, ref := range runtimeRefs(j.ServiceStates["web"].Candidate) {
		if f.docker.containers[ref.Container] != nil || *ref.Port != "" || *ref.PublishPort == "" {
			t.Fatal("candidate not recreatable after cancellation")
		}
	}
	badPort = ""
	if err := resumeReplicas(f, op); err != nil {
		t.Fatal(err)
	}
}

func TestReplicaOwnershipAndDegradedStatus(t *testing.T) {
	f := replicaFlow(t)
	j := f.load()
	r := j.ServiceStates["web"].Current
	ref := runtimeRefs(r)[1]
	v := f.docker.containers[ref.Container]
	v.Config.Labels["soar.replica"] = "1"
	if _, err := f.m.instanceInspect(context.Background(), j, ref, false, true); err == nil {
		t.Fatal("wrong member label authorized")
	}
	if err := validateMultiEndpoint(j, v); err == nil {
		t.Fatal("network admitted wrong member label")
	}
	v.Config.Labels["soar.replica"] = "2"
	v.State.Running = false
	q := Request{Version: 1, Action: "status", App: j.App, Host: f.req.Host}
	resp, err := f.m.Execute(context.Background(), q, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := resp.Status.Services["web"]
	if s.Running || s.Available || s.Container != "" || s.RunningInstances != 2 || s.AvailableInstances != 2 || len(s.Instances) != 3 {
		t.Fatalf("degraded group misreported: %+v", s)
	}
}

func TestReplicaJournalVolumeSingleton(t *testing.T) {
	f := newMultiFlow(t)
	f.req.Partial = true
	a := replicaApp(2)
	db := volumeFlowApp("one").Services["backend"]
	a.Services["backend"] = db
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if len(j.ServiceStates["backend"].Current.Instances) != 1 {
		t.Fatal("v8 volume singleton lacks indexed identity")
	}
	f.req.HostIdentity = j.HostID
	f.req.Bindings = map[string]string{}
	for name, s := range j.ServiceStates {
		f.req.Bindings[name] = s.Incarnation
	}
	db.Env = map[string]string{"REVISION": "two"}
	a.Services["backend"] = db
	if _, err := f.deploy(a, strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	old := runtimeRefs(j.ServiceStates["backend"].Current)[0]
	if f.docker.containers[old.Container].State.Running {
		t.Fatal("old volume writer running")
	}
}

func TestReplicaJournalRejectsContradictions(t *testing.T) {
	f := replicaFlow(t)
	for _, mutate := range []func(*journal){
		func(j *journal) { j.Version = 7 },
		func(j *journal) { j.ServiceStates["web"].Current.Container = "scalar" },
		func(j *journal) { j.ServiceStates["web"].Current.Instances[1].Ordinal = 1 },
		func(j *journal) { j.ServiceStates["web"].Current.Instances[1].Container += "-extra" },
		func(j *journal) {
			j.ServiceStates["web"].Current.Instances[1].PublishPort = j.ServiceStates["web"].Current.Instances[0].PublishPort
		},
	} {
		j := f.load()
		mutate(j)
		if validateMultiJournal(j, j.App) == nil {
			t.Fatal("corrupt group accepted")
		}
	}
	for _, version := range []int{0, 9, 99} {
		j := &journal{Version: version}
		if isMulti(j) || supportsVolumes(j) || supportsServiceIdentity(j) {
			t.Fatal("unbounded version capability")
		}
	}
}

func TestReplicaSingletonCandidateStatus(t *testing.T) {
	f := replicaFlow(t)
	run := f.m.Run
	f.m.Run = func(c context.Context, n string, args []string, b []byte) (process.Result, error) {
		if n == "docker" && args[0] == "run" && strings.Contains(strings.Join(args, " "), "soar.service=web") {
			return process.Result{}, errors.New("interrupted singleton creation")
		}
		return run(c, n, args, b)
	}
	if _, err := f.deploy(replicaApp(1), strings.Repeat("2", 32)); err == nil {
		t.Fatal("expected interrupted scale-down")
	}
	j := f.load()
	want := j.ServiceStates["web"].Candidate.Instances[0].Container
	resp, err := f.m.Execute(context.Background(), Request{Version: 1, Action: "status", App: j.App, Host: f.req.Host}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []Status{*resp.Status, maintenanceStatus(j, "example.com")} {
		if status.Services["web"].Candidate != want {
			t.Fatalf("singleton candidate compatibility identity missing: %+v", status.Services["web"])
		}
	}
}

func TestReplicaSnapshotLogsPinDockerID(t *testing.T) {
	f := replicaFlow(t)
	j := f.load()
	q := Request{Version: 1, Action: "logs", App: j.App, Host: f.req.Host, Service: "web"}
	if _, err := f.m.Execute(context.Background(), q, nil); err == nil {
		t.Fatal("ambiguous snapshot logs accepted")
	}
	q.Replica = 2
	ref := runtimeRefs(j.ServiceStates["web"].Current)[1]
	id := f.docker.containers[ref.Container].ID
	run := f.m.Run
	called := false
	f.m.Run = func(c context.Context, n string, args []string, b []byte) (process.Result, error) {
		if n == "docker" && args[0] == "logs" {
			called = true
			if args[len(args)-1] != id {
				t.Fatalf("logs addressed mutable name: %v", args)
			}
			return process.Result{Stdout: []byte("selected member")}, nil
		}
		return run(c, n, args, b)
	}
	resp, err := f.m.Execute(context.Background(), q, nil)
	if err != nil || !called || resp.LogContainer != ref.Container || resp.Logs != "selected member" {
		t.Fatalf("%+v %v", resp, err)
	}
}

func TestReplicaLegacySnapshotLogsPinDockerID(t *testing.T) {
	f, q := flowSetup(t)
	j := f.execute(q, "legacy", false)
	id := strings.Repeat("d", 64)
	f.containers[j.Current.Container].ID = id
	run := f.m.Run
	called := false
	f.m.Run = func(c context.Context, n string, args []string, b []byte) (process.Result, error) {
		if n == "docker" && args[0] == "logs" {
			called = true
			if args[len(args)-1] != id {
				t.Fatalf("legacy logs addressed mutable name: %v", args)
			}
			return process.Result{Stdout: []byte("legacy logs")}, nil
		}
		return run(c, n, args, b)
	}
	q = Request{Version: 1, Action: "logs", App: j.App, Host: q.Host, Replica: 1}
	resp, err := f.m.Execute(context.Background(), q, nil)
	if err != nil || !called || resp.Logs != "legacy logs" || resp.LogContainer != j.Current.Container {
		t.Fatalf("%+v %v", resp, err)
	}
}
