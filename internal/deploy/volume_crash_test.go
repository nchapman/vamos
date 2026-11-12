package deploy

import (
	"context"
	"strings"
	"testing"
)

func TestVolumeStartupCrashLoopCanBeSuperseded(t *testing.T) {
	f := newMultiFlow(t)
	f.failHealth["web"] = true
	failed := strings.Repeat("1", 32)
	if _, err := f.deploy(volumeFlowApp("broken"), failed); err == nil {
		t.Fatal("expected readiness failure")
	}
	candidate := f.load().ServiceStates["web"].Candidate
	v := f.docker.containers[candidate.Container]
	// Docker reports Running+Restarting during restart backoff, without effective
	// network ports. Static captured bindings still identify the container safely.
	v.State.Running = true
	v.State.Restarting = true
	v.NetworkSettings.Ports = nil
	f.failHealth["web"] = false
	request := f.req
	request.Config = volumeFlowApp("fixed")
	request.Operation = strings.Repeat("2", 32)
	request.Supersede = failed
	if _, err := f.m.Execute(context.Background(), request, f.bundle(request.Config)); err != nil {
		t.Fatal("cannot recover crashed volume writer:", err)
	}
	if f.docker.containers[candidate.Container] != nil || f.load().Phase != "ready" {
		t.Fatal("crashed candidate was not replaced")
	}
}
