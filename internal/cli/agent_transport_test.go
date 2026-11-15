package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
)

func TestRunAppTransportPrefersAgentAndRejectsExecutorOverride(t *testing.T) {
	request := deploy.Request{Version: 1, Action: "status", App: "billing", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}}
	var legacyCalled bool
	deps := appDeps{
		Run: func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
			legacyCalled = true
			return deploy.Response{}, nil
		},
		Agent: func(ctx context.Context, target string, r deploy.Request, payload []byte) (deploy.Response, error) {
			if target != "root@home" || r.App != "billing" || payload != nil {
				t.Fatalf("unexpected agent transport call: %s %+v", target, r)
			}
			return deploy.Response{Version: 1, Status: &deploy.Status{App: "billing"}}, nil
		},
	}
	response, err := runAppTransport(context.Background(), "root@home", "", request, nil, deps)
	if err != nil || response.Status == nil || legacyCalled {
		t.Fatalf("agent path not preferred: %+v %v", response, err)
	}
	if _, err := runAppTransport(context.Background(), "root@home", "executor.bin", request, nil, deps); err == nil || !strings.Contains(err.Error(), "--executor override") {
		t.Fatalf("override accepted on agent path: %v", err)
	}
}

func TestRunAppTransportLegacyPathInspectsRootAndUploads(t *testing.T) {
	request := deploy.Request{Version: 1, Action: "status", App: "billing", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}}
	var ran bool
	deps := appDeps{
		Inspect: func(context.Context, string) (*host.Report, error) {
			return &host.Report{Supported: true, Root: true, Architecture: "amd64"}, nil
		},
		Artifact: func(override, arch string) ([]byte, error) {
			if override != "" || arch != "amd64" {
				t.Fatalf("artifact %s %s", override, arch)
			}
			return []byte("binary"), nil
		},
		Run: func(_ context.Context, target string, r deploy.Request, binary, payload []byte) (deploy.Response, error) {
			ran = true
			if target != "root@home" || string(binary) != "binary" {
				t.Fatalf("unexpected legacy call: %s", target)
			}
			return deploy.Response{Version: 1, Status: &deploy.Status{App: r.App}}, nil
		},
	}
	response, err := runAppTransport(context.Background(), "root@home", "", request, nil, deps)
	if err != nil || !ran || response.Status == nil {
		t.Fatalf("legacy path broken: %+v %v", response, err)
	}
	deps.Inspect = func(context.Context, string) (*host.Report, error) { return &host.Report{}, nil }
	if _, err := runAppTransport(context.Background(), "root@home", "", request, nil, deps); err == nil || !strings.Contains(err.Error(), "requires root") {
		t.Fatalf("non-root host accepted: %v", err)
	}
}

func TestRunAppTransportLegacyFailure(t *testing.T) {
	request := deploy.Request{Version: 1, Action: "status", App: "billing", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}}
	deps := appDeps{
		Inspect: func(context.Context, string) (*host.Report, error) {
			return &host.Report{Supported: true, Root: true, Architecture: "amd64"}, nil
		},
		Artifact: func(string, string) ([]byte, error) { return []byte("binary"), nil },
		Run: func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
			return deploy.Response{}, errors.New("ssh down")
		},
	}
	if _, err := runAppTransport(context.Background(), "root@home", "", request, nil, deps); err == nil {
		t.Fatal("legacy failure swallowed")
	}
}

func TestRunSessionTransportPrefersAgentStream(t *testing.T) {
	request := deploy.SessionRequest{Version: 1, Action: "logs", App: "billing", Service: "web", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}, Tail: 10}
	deps := appDeps{
		AgentStream: func(ctx context.Context, target string, r deploy.SessionRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			if target != "root@home" || r.App != "billing" {
				t.Fatalf("unexpected agent stream call: %s %+v", target, r)
			}
			return 3, nil
		},
	}
	code, err := runSessionTransport(context.Background(), "root@home", "", request, nil, &bytes.Buffer{}, &bytes.Buffer{}, deps)
	if err != nil || code != 3 {
		t.Fatalf("agent stream not preferred: %d %v", code, err)
	}
	if _, err := runSessionTransport(context.Background(), "root@home", "executor.bin", request, nil, &bytes.Buffer{}, &bytes.Buffer{}, deps); err == nil || !strings.Contains(err.Error(), "--executor override") {
		t.Fatalf("override accepted on agent stream: %v", err)
	}
}
