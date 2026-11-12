package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/process"
)

func TestVolumeMetadataFailurePreservesLogsAndReportsUnverifiedStorage(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(volumeFlowApp("one"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	response := Response{Logs: "captured application logs", Status: &Status{App: j.App, LastError: "original operation diagnostic", Services: map[string]ServiceStatus{"web": {Running: true, Available: true}}}}
	f.m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{}, errors.New("private-runtime-canary")
	}
	f.m.enrichVolumeResponse(context.Background(), Request{App: j.App}, j.HostID, &response)
	if response.Logs != "captured application logs" || !response.Status.Services["web"].Running || response.Status.Services["web"].Available || response.Status.Services["web"].Unavailable || !strings.Contains(response.Status.LastError, "original operation diagnostic") {
		t.Fatal("metadata changed logs, process observation, or route state", response)
	}
}

func TestVolumeMetadataFailureDoesNotFailStatus(t *testing.T) {
	m, j, _ := volumeBoundaryFixture(t)
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{}, errors.New("private-runtime-canary")
	}
	request := Request{Version: 1, Action: "status", App: j.App, Host: lifecycle.Config{Name: "host", Domain: "example.com"}}
	response, err := m.Execute(context.Background(), request, nil)
	if err != nil {
		t.Fatal("metadata enrichment changed successful action outcome:", err)
	}
	if response.Status == nil || !strings.Contains(response.Status.LastError, "volume metadata inspection failed") || strings.Contains(response.Status.LastError, "private-runtime-canary") {
		t.Fatalf("missing safe diagnostic: %+v", response.Status)
	}
}
