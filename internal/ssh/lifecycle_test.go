package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

func TestLifecycleArchiveAndFixedCommand(t *testing.T) {
	binary := []byte("binary\x00payload")
	var previous []string
	for _, name := range []string{"home", "quote'; $(touch /tmp/not-executed)\n"} {
		request := executor.Request{Version: 1, Operation: "bootstrap", Confirm: true, Config: &lifecycle.Config{Name: name, Domain: "example.com"}}
		ctx := context.Background()
		state, err := runLifecycle(ctx, "root@home", request, binary, func(got context.Context, exe string, args []string, input []byte) (process.Result, error) {
			if got != ctx || exe != "ssh" || !reflect.DeepEqual(args[len(args)-3:len(args)-1], []string{"--", "root@home"}) {
				t.Fatalf("invocation %s %q", exe, args)
			}
			if previous != nil && !reflect.DeepEqual(previous, args) {
				t.Fatal("request changed remote command")
			}
			previous = append([]string(nil), args...)
			wantCommand := "/bin/sh -c '" + strings.ReplaceAll(bootstrapScript, "'", "'\\''") + "'"
			if args[len(args)-1] != adminCommand(wantCommand) {
				t.Fatal("not the fixed bootstrap command")
			}
			tr := tar.NewReader(bytes.NewReader(input))
			encoded, _ := json.Marshal(request)
			hash := sha256.Sum256(binary)
			for _, want := range []struct {
				name string
				mode int64
				data []byte
			}{{"executor", 0700, binary}, {"request.json", 0600, encoded}, {"checksum", 0600, []byte(fmt.Sprintf("%x  executor\n", hash))}} {
				h, e := tr.Next()
				if e != nil {
					t.Fatal(e)
				}
				data, e := io.ReadAll(tr)
				if e != nil || h.Name != want.name || h.Mode != want.mode || h.Typeflag != tar.TypeReg || !bytes.Equal(data, want.data) {
					t.Fatalf("archive header=%+v data=%q err=%v", h, data, e)
				}
			}
			if _, e := tr.Next(); e != io.EOF {
				t.Fatalf("extra archive entry: %v", e)
			}
			return process.Result{Stdout: []byte(`{"version":1,"host_state":{"version":1,"phase":"ready","id":"installation"}}`)}, nil
		})
		if err != nil || state.Phase != "ready" || state.ID != "installation" {
			t.Fatalf("state=%+v err=%v", state, err)
		}
	}
}

func TestLifecycleTransportErrors(t *testing.T) {
	failure := errors.New("connection lost")
	for _, tt := range []struct {
		name, output, want string
		runErr             error
	}{
		{"malformed", "banner", "invalid lifecycle response", nil},
		{"version", `{"version":2,"host_state":{}}`, "invalid lifecycle response", nil},
		{"missing state", `{"version":1}`, "invalid lifecycle response", nil},
		{"unknown field", `{"version":1,"extra":true}`, "invalid lifecycle response", nil},
		{"multiple responses", `{"version":1} {}`, "invalid lifecycle response", nil},
		{"remote failure", `{"version":1,"error":{"code":"lifecycle_failed","message":"bad host"}}`, "remote bootstrap failed", failure},
		{"disconnect", "", "outcome may be unknown", failure},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state, err := runLifecycle(context.Background(), "home", executor.Request{Version: 1, Operation: "bootstrap"}, []byte("binary"), func(context.Context, string, []string, []byte) (process.Result, error) {
				return process.Result{Stdout: []byte(tt.output)}, tt.runErr
			})
			if state != nil || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			if tt.name == "disconnect" && !errors.Is(err, failure) {
				t.Fatal("lost connection cause")
			}
		})
	}
}

func TestLifecycleRejectsBeforeRunner(t *testing.T) {
	for _, tt := range []struct {
		target  string
		request executor.Request
		binary  []byte
	}{
		{"host;id", executor.Request{}, []byte("binary")},
		{"home", executor.Request{}, nil},
		{"home", executor.Request{Operation: strings.Repeat("x", 4096)}, []byte("binary")},
	} {
		_, err := runLifecycle(context.Background(), tt.target, tt.request, tt.binary, func(context.Context, string, []string, []byte) (process.Result, error) {
			t.Fatal("invalid input reached SSH")
			return process.Result{}, nil
		})
		if err == nil {
			t.Fatal("invalid input accepted")
		}
	}
}

func TestTransportScriptsExecuteOutsideNoexecRun(t *testing.T) {
	for name, script := range map[string]string{
		"bootstrap":       bootstrapScript,
		"app":             appScript,
		"app-read":        appReadScript,
		"session-prepare": sessionPrepareScript,
	} {
		if strings.Contains(script, `"$stage/executor" internal-`) || strings.Contains(script, `./executor`) {
			t.Fatalf("%s still executes uploaded binary directly from /run staging:\n%s", name, script)
		}
		if !strings.Contains(script, "/var/lib/.vamos-") {
			t.Fatalf("%s does not move executable out of /run staging:\n%s", name, script)
		}
	}
}
