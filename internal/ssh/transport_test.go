package ssh

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
)

func TestTransportInspectsSelectsArtifactAndRuns(t *testing.T) {
	var seenOverride, seenArch string
	tr := &Transport{
		ExecutorOverride: "/tmp/custom-exec",
		InspectFn: func(context.Context, string) (*host.Report, error) {
			return &host.Report{Supported: true, Root: true, Architecture: "arm64"}, nil
		},
		ArtifactFn: func(override, arch string) ([]byte, error) {
			seenOverride, seenArch = override, arch
			return []byte("verified-exec"), nil
		},
		RunAppFn: func(_ context.Context, target string, req deploy.Request, binary, payload []byte) (deploy.Response, error) {
			if target != "root@host" || string(binary) != "verified-exec" || string(payload) != "ctx" || req.App != "app" {
				t.Fatalf("unexpected RunApp args: target=%s binary=%q payload=%q", target, binary, payload)
			}
			return deploy.Response{Version: 1}, nil
		},
		StreamAppFn: func(_ context.Context, target string, _ deploy.SessionRequest, binary []byte, _ io.Reader, _, _ io.Writer) (int, error) {
			if target != "root@host" || string(binary) != "verified-exec" {
				t.Fatal("unexpected StreamApp args")
			}
			return 0, nil
		},
	}

	req := deploy.Request{Version: 1, Action: "status", App: "app", Host: lifecycle.Config{Name: "h", Domain: "example.com"}}
	resp, err := tr.RunApp(context.Background(), "root@host", req, []byte("ctx"))
	if err != nil || resp.Version != 1 || seenOverride != "/tmp/custom-exec" || seenArch != "arm64" {
		t.Fatalf("RunApp failed: resp=%+v err=%v override=%s arch=%s", resp, err, seenOverride, seenArch)
	}

	code, err := tr.StreamApp(context.Background(), "root@host", deploy.SessionRequest{Version: 1}, nil, io.Discard, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("StreamApp failed: code=%d err=%v", code, err)
	}

	for _, badReport := range []*host.Report{
		{Supported: true, Root: false, Architecture: "arm64"},
		{Supported: false, Root: true, Architecture: "arm64"},
	} {
		tr.InspectFn = func(context.Context, string) (*host.Report, error) { return badReport, nil }
		want := "requires root"
		if !badReport.Supported {
			want = "unsupported host"
		}
		if _, err := tr.RunApp(context.Background(), "root@host", req, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("expected RunApp host refusal for %+v, got %v", badReport, err)
		}
		if code, err := tr.StreamApp(context.Background(), "root@host", deploy.SessionRequest{Version: 1}, nil, io.Discard, io.Discard); code != -1 || err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("expected StreamApp host refusal for %+v, got code=%d err=%v", badReport, code, err)
		}
	}

	tr.InspectFn = func(context.Context, string) (*host.Report, error) {
		return nil, errors.New("ssh unreachable")
	}
	if _, err := tr.RunApp(context.Background(), "root@host", req, nil); err == nil || !strings.Contains(err.Error(), "ssh unreachable") {
		t.Fatalf("expected RunApp inspect error, got %v", err)
	}
	if code, err := tr.StreamApp(context.Background(), "root@host", deploy.SessionRequest{Version: 1}, nil, io.Discard, io.Discard); code != -1 || err == nil || !strings.Contains(err.Error(), "ssh unreachable") {
		t.Fatalf("expected StreamApp inspect error, got code=%d err=%v", code, err)
	}

	tr.InspectFn = func(context.Context, string) (*host.Report, error) {
		return &host.Report{Supported: true, Root: true, Architecture: "arm64"}, nil
	}
	tr.ArtifactFn = func(string, string) ([]byte, error) { return nil, errors.New("mismatch") }
	if _, err := tr.RunApp(context.Background(), "root@host", req, nil); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected RunApp artifact refusal, got %v", err)
	}
	if code, err := tr.StreamApp(context.Background(), "root@host", deploy.SessionRequest{Version: 1}, nil, io.Discard, io.Discard); code != -1 || err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected StreamApp artifact refusal, got code=%d err=%v", code, err)
	}
}
