package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/process"
)

func appRequest() deploy.Request {
	return deploy.Request{Version: 1, Action: "deploy", App: "hello", Operation: strings.Repeat("a", 32), Host: lifecycle.Config{Name: "home", Domain: "example.com"}, Config: &config.App{Name: "hello", Services: map[string]config.Service{"web": {Build: &config.Build{Context: ".", Dockerfile: "Dockerfile"}, HTTP: &config.HTTP{Port: 80, Health: "/healthz"}, Update: config.UpdateAuto, Env: map[string]string{"LITERAL": "$(touch nope); 'quoted'"}}}}}
}

func TestAppTransportUsesFixedCommandAndBoundedTar(t *testing.T) {
	request := appRequest()
	run := func(_ context.Context, name string, args []string, input []byte) (process.Result, error) {
		if name != "ssh" {
			t.Fatal(name)
		}
		command := args[len(args)-1]
		if strings.Contains(command, request.Config.Services["web"].Env["LITERAL"]) || !strings.Contains(command, "internal-app-executor") {
			t.Fatal("request interpolated into shell")
		}
		files := map[string][]byte{}
		reader := tar.NewReader(bytes.NewReader(input))
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if header.Typeflag != tar.TypeReg {
				t.Fatal("unexpected entry")
			}
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			files[header.Name] = data
			if header.Mode&0077 != 0 {
				t.Fatal("public archive entry")
			}
		}
		if len(files) != 4 || string(files["context.tar"]) != "context" || string(files["executor"]) != "binary" {
			t.Fatalf("files=%v", files)
		}
		var decoded deploy.Request
		if err := json.Unmarshal(files["request.json"], &decoded); err != nil || decoded.Config.Services["web"].Env["LITERAL"] != request.Config.Services["web"].Env["LITERAL"] {
			t.Fatal("lost literal configuration", err)
		}
		data, _ := json.Marshal(deploy.Response{Version: 1, Status: &deploy.Status{App: "hello", Phase: "ready", Operation: request.Operation}})
		return process.Result{Stdout: data}, nil
	}
	if _, err := runApp(context.Background(), "root@home", request, []byte("binary"), []byte("context"), run); err != nil {
		t.Fatal(err)
	}
}

func TestAppQueriesUploadMatchingExecutorToTemporaryStage(t *testing.T) {
	for _, action := range []string{"status", "logs"} {
		r := appRequest()
		r.Action = action
		r.Config = nil
		r.Operation = ""
		run := func(_ context.Context, _ string, args []string, input []byte) (process.Result, error) {
			command := args[len(args)-1]
			if strings.Contains(command, "systemd-run") || strings.Contains(command, "/usr/local/lib/soar") || !strings.Contains(command, "mktemp") || !strings.Contains(command, "sha256sum") || !strings.Contains(command, "trap") {
				t.Fatal("read query started worker")
			}
			files := map[string][]byte{}
			tr := tar.NewReader(bytes.NewReader(input))
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				files[h.Name], err = io.ReadAll(tr)
				if err != nil {
					t.Fatal(err)
				}
			}
			if string(files["executor"]) != "binary" {
				t.Fatal("missing executor")
			}
			var got deploy.Request
			if err := json.Unmarshal(files["request.json"], &got); err != nil || got.Action != action {
				t.Fatal("request", err)
			}
			data, _ := json.Marshal(deploy.Response{Version: 1, Status: &deploy.Status{App: "hello"}, Logs: "log\n"})
			return process.Result{Stdout: data}, nil
		}
		if _, err := runApp(context.Background(), "root@home", r, []byte("binary"), nil, run); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAppTransportRejectsInvalidAndFailedResponses(t *testing.T) {
	for _, data := range []string{"", `null`, `{"version":2}`, `{"version":1}`, `{"version":1,"error":"failed"}`, `{"version":1,"status":{}} {}`} {
		run := func(context.Context, string, []string, []byte) (process.Result, error) {
			return process.Result{Stdout: []byte(data)}, nil
		}
		if _, err := runApp(context.Background(), "root@home", appRequest(), []byte("binary"), []byte("context"), run); err == nil {
			t.Errorf("accepted %q", data)
		}
	}
	run := func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{}, errors.New("lost SSH")
	}
	if _, err := runApp(context.Background(), "root@home", appRequest(), []byte("binary"), []byte("context"), run); err == nil || !strings.Contains(err.Error(), "outcome may be unknown") {
		t.Fatal(err)
	}
	if _, err := runApp(context.Background(), "bad;target", appRequest(), nil, nil, nil); err == nil {
		t.Fatal("unsafe target accepted")
	}
}
