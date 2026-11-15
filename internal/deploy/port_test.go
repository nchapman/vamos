package deploy

import (
	"context"
	"errors"
	"github.com/nchapman/vamos/internal/process"
	"strings"
	"testing"
)

func TestDeploymentPinsConfiguredPort(t *testing.T) {
	f, r := flowSetup(t)
	j := f.execute(r, "context", false)
	v := f.containers[j.Current.Container]
	p := v.HostConfig.PortBindings["8080/tcp"]
	if len(p) != 1 || p[0].HostPort == "" || p[0].HostPort != j.Current.Port {
		t.Fatalf("deployment accepted dynamic binding: configured=%v endpoint=%s", p, j.Current.Port)
	}
	v.HostConfig.PortBindings["8080/tcp"][0].HostPort = ""
	if _, err := f.m.verifyCandidate(context.Background(), j, j.Current); err == nil {
		t.Fatal("accepted dynamic binding in place of pinned intent")
	}
	if !strings.Contains(strings.Join(f.events, "\n"), "--publish 127.0.0.1:"+j.Current.Port+":8080") {
		t.Fatal("Docker create did not use recorded endpoint")
	}
}

func TestPinnedEndpointRecreationUsesSameIntent(t *testing.T) {
	f, r := flowSetup(t)
	f.unhealthy = true
	j := f.execute(r, "context", true)
	port := j.Candidate.PublishPort
	delete(f.containers, j.Candidate.Container)
	f.m.AllocatePort = func() (string, error) { t.Fatal("reallocated persisted intent"); return "", nil }
	f.unhealthy = false
	j = f.execute(flowResume(r), "", false)
	if j.Current.Port != port || j.Current.PublishPort != port {
		t.Fatal("changed endpoint on recreation")
	}
}

func TestPortIntentPersistedBeforeCreateAndNeverReallocated(t *testing.T) {
	f, r := flowSetup(t)
	run := f.m.Run
	f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "docker" && a[0] == "run" {
			j, err := f.m.load(r.App)
			if err != nil {
				t.Fatal(err)
			}
			if j.Candidate.PublishPort != "12000" {
				t.Fatal("create before durable intent")
			}
			return process.Result{}, errors.New("bind conflict")
		}
		return run(c, n, a, b)
	}
	j := f.execute(r, "context", true)
	if j.Candidate.PublishPort != "12000" {
		t.Fatal("lost intent after bind failure")
	}
	f.m.AllocatePort = func() (string, error) { t.Fatal("reallocation after bind failure"); return "", nil }
	f.execute(flowResume(r), "", true)
	f.m.Run = run
	f.execute(flowResume(r), "", false)
}

func TestPinnedPortDriftReportedByStatus(t *testing.T) {
	f, r := flowSetup(t)
	j := f.execute(r, "context", false)
	f.containers[j.Current.Container].NetworkSettings.Ports["8080/tcp"][0].HostPort = "65534"
	r.Action, r.Operation, r.Config = "status", "", nil
	response, err := f.m.Execute(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status.Running || !strings.Contains(response.Status.LastError, "effective port") {
		t.Fatalf("drift not reported: %+v", response.Status)
	}
}
