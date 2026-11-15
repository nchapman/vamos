package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/deploy"
)

func TestReuseDataCLI(t *testing.T) {
	for _, action := range []string{"status", "logs", "resume", "cleanup", "destroy"} {
		for _, flag := range []string{"--reuse-data", "--reuse-data=false"} {
			var out, diag bytes.Buffer
			if code := appCommand(context.Background(), action, []string{flag}, &out, &diag, appDeps{}); code != 2 || !strings.Contains(diag.String(), "deploy only") {
				t.Fatalf("%s %s: %d %s", action, flag, code, &diag)
			}
		}
	}
	for _, flag := range []string{"--reuse-data", "--reuse-data=false"} {
		path, _, deps := appFixture(t)
		deps.Run = func(_ context.Context, _ string, r deploy.Request, _, _ []byte) (deploy.Response, error) {
			if r.ReuseData != (flag == "--reuse-data") {
				t.Fatalf("reuse not forwarded: %+v", r)
			}
			return deploy.Response{Status: &deploy.Status{Phase: "ready"}}, nil
		}
		var out, diag bytes.Buffer
		if code := appCommand(context.Background(), "deploy", []string{"-f", path, flag}, &out, &diag, deps); code != 0 {
			t.Fatalf("%d: %s", code, &diag)
		}
		if strings.Contains(out.String(), "Data is retained") {
			t.Fatal("stateless deploy claimed data")
		}
	}
}

func TestVolumeWarningBeforeRemoteMutation(t *testing.T) {
	path, _, deps := appFixture(t)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("    volume: {name: data, mount: /data}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var out, diag bytes.Buffer
	deps.Run = func(_ context.Context, _ string, r deploy.Request, _, _ []byte) (deploy.Response, error) {
		for _, want := range []string{`service="web"`, `name="data"`, `mount="/data"`, "stop/start", "HTTP 503", "Data is retained", "may change it", "no automatic rollback", "identity resolved remotely"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("missing warning %q: %s", want, &out)
			}
		}
		return deploy.Response{Status: &deploy.Status{Phase: "ready"}}, nil
	}
	if code := appCommand(context.Background(), "deploy", []string{"-f", path}, &out, &diag, deps); code != 0 {
		t.Fatalf("%d: %s", code, &diag)
	}
}

func TestCapturedVolumeWarningsAreConditional(t *testing.T) {
	for _, action := range []string{"resume", "deploy"} {
		var out bytes.Buffer
		if err := printVolumeWarnings(&out, action, strings.Repeat("a", 32), nil); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"If the captured operation uses volumes", "may already have changed", "No automatic rollback", "HTTP 503"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %q: %s", want, &out)
			}
		}
	}
}

func TestVolumeStatusMetadata(t *testing.T) {
	for _, phase := range []string{"ready", "staged", "destroyed"} {
		for _, asJSON := range []bool{false, true} {
			path, _, deps := appFixture(t)
			status := &deploy.Status{Phase: phase, Volumes: []deploy.VolumeStatus{{Service: "web", Name: "data", Mount: "/data", ID: "remote-id", State: "retained", Missing: true, DataMayHaveChanged: true}}, Services: map[string]deploy.ServiceStatus{"web": {Kind: "public", Unavailable: true, DataMayHaveChanged: true}}}
			deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
				return deploy.Response{Status: status}, nil
			}
			args := []string{"-f", path}
			if asJSON {
				args = append(args, "--json")
			}
			var out, diag bytes.Buffer
			if code := appCommand(context.Background(), "status", args, &out, &diag, deps); code != 0 {
				t.Fatalf("%d: %s", code, &diag)
			}
			if asJSON {
				var got deploy.Status
				if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Volumes) != 1 || got.Volumes[0] != status.Volumes[0] {
					t.Fatalf("JSON metadata: %s (%v)", &out, err)
				}
			} else {
				for _, want := range []string{`service="web"`, `name="data"`, `mount="/data"`, `id="remote-id"`, `state="retained"`, "missing=true", "data-may-have-changed=true"} {
					if !strings.Contains(out.String(), want) {
						t.Errorf("missing %q: %s", want, &out)
					}
				}
				if phase == "ready" && !strings.Contains(out.String(), "unavailable=true") {
					t.Fatal("unavailable marker missing")
				}
			}
			if strings.Contains(out.String(), "contents") {
				t.Fatal("status must expose metadata only")
			}
		}
	}
}
