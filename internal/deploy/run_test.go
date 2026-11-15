package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

func TestRunDurableBoundary(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed-command", true: "lost-start-response"}[lost], func(t *testing.T) {
			f := newMultiFlow(t)
			if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			j := f.load()
			stageMultiSecret(t, f, strings.Repeat("b", 32), "latest-run-secret")
			q := f.req
			q.Action = "run"
			q.Operation = strings.Repeat("2", 32)
			q.Service = "backend"
			q.Command = []string{"./migrate", "space argument", "--literal=$(id)"}
			a := multiApp()
			cfg := a.Services["backend"]
			cfg.Secrets = map[string]string{"TOKEN": "credential"}
			a.Services["backend"] = cfg
			other := a.Services["web"]
			other.Secrets = map[string]string{"MISSING": "not-staged"}
			a.Services["web"] = other
			q.Config = a
			selected := &config.App{Name: j.App, Services: map[string]config.Service{"backend": cfg}}
			state, err := f.m.secretStore().Load(j.App, j.HostID)
			if err != nil {
				t.Fatal(err)
			}
			old := f.m.Run
			starts := 0
			var create []string
			completed := false
			f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
				if name == "docker" {
					if args[0] == "create" {
						create = append([]string(nil), args...)
					}
					if args[0] == "start" {
						starts++
						v := f.docker.containers["vamos-run-"+q.Operation]
						if v == nil || args[1] != v.ID {
							t.Fatal(args)
						}
						v.State.Running = true
						if lost {
							return process.Result{}, errors.New("response lost")
						}
						return process.Result{}, nil
					}
					if args[0] == "wait" {
						completed = true
						f.docker.containers["vamos-run-"+q.Operation].State.Running = false
						return process.Result{Stdout: []byte("7\n")}, nil
					}
					if args[0] == "logs" {
						return process.Result{Stdout: []byte(strings.Repeat("x", 20000))}, nil
					}
					if len(args) == 3 && args[0] == "container" && args[1] == "inspect" && completed {
						v := f.docker.containers["vamos-run-"+q.Operation]
						if args[2] == v.ID || args[2] == strings.TrimPrefix(v.Name, "/") {
							b, _ := json.Marshal(v)
							var obj map[string]any
							_ = json.Unmarshal(b, &obj)
							st := obj["State"].(map[string]any)
							st["Status"] = "exited"
							st["ExitCode"] = 7
							st["FinishedAt"] = "2026-06-01T00:01:00Z"
							return dockerJSON([]any{obj})
						}
					}
				}
				return old(ctx, name, args, input)
			}
			// Build image ENTRYPOINT survives command replacement.
			f.docker.beforeRun = func(args []string) {
				if args[0] == "create" {
					for _, arg := range args {
						if im := f.docker.images[arg]; im != nil {
							im.Config.Entrypoint = []string{"/entrypoint"}
						}
					}
				}
			}
			before, _ := os.ReadFile(filepath.Join(f.m.dir(j.App), "state.json"))
			resp, err := f.m.executeRun(context.Background(), q, f.bundle(selected), j, &lifecycle.State{ID: j.HostID}, state)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Run == nil {
				t.Fatal("missing result")
			}
			if !lost && (resp.Run.ExitCode == nil || *resp.Run.ExitCode != 7 || len(resp.Run.Output) != 8192) {
				t.Fatalf("%+v", resp.Run)
			}
			if lost && resp.Run.Phase != "running" {
				t.Fatalf("%+v", resp.Run)
			}
			if !reflect.DeepEqual(create[len(create)-len(q.Command):], q.Command) {
				t.Fatal(create)
			}
			for _, forbidden := range []string{"--publish", "--mount", "--network-alias", "--entrypoint", "--rm"} {
				for _, arg := range create {
					if arg == forbidden {
						t.Fatal(create)
					}
				}
			}
			v := f.docker.containers["vamos-run-"+q.Operation]
			if !reflect.DeepEqual(v.Config.Entrypoint, []string{"/entrypoint"}) {
				t.Fatal(v.Config)
			}
			if !strings.Contains(strings.Join(v.Config.Env, "\n"), "TOKEN=latest-run-secret") {
				t.Fatal("selected secret missing")
			}
			after, _ := os.ReadFile(filepath.Join(f.m.dir(j.App), "state.json"))
			if string(before) != string(after) {
				t.Fatal("active journal modified")
			}
			if _, err = f.m.executeRun(context.Background(), q, nil, j, &lifecycle.State{ID: j.HostID}, state); err != nil || starts != 1 {
				t.Fatal("duplicate restarted", err, starts)
			}
			if err = f.m.verifyRunEndpoint(context.Background(), j, v); err != nil {
				t.Fatal(err)
			}
			if !lost {
				t.Run("reconcile lost result save without restart", func(t *testing.T) {
					rec, err := f.m.loadRun(j, q.Operation)
					if err != nil {
						t.Fatal(err)
					}
					rec.Phase, rec.ExitCode, rec.Output = "start-intent", nil, ""
					if err := f.m.saveRun(j, rec); err != nil {
						t.Fatal(err)
					}
					if err := f.m.preflightRuns(context.Background(), j); err != nil {
						t.Fatal(err)
					}
					rec, err = f.m.loadRun(j, q.Operation)
					if err != nil || rec.Phase != "complete" || rec.ExitCode == nil || *rec.ExitCode != 7 || starts != 1 {
						t.Fatalf("outcome not durably reconciled: %+v %v starts=%d", rec, err, starts)
					}
				})
			}
			v.Config.Cmd = []string{"foreign"}
			if f.m.verifyRunEndpoint(context.Background(), j, v) == nil {
				t.Fatal("accepted command drift")
			}
			v.Config.Cmd = q.Command
			v.ID = strings.Repeat("f", 64)
			if f.m.verifyRunEndpoint(context.Background(), j, v) == nil {
				t.Fatal("accepted foreign ID")
			}
			if lost {
				// A saved start intent with a stopped, never-started container is
				// uncertain, not permission to retry start.
				rec, e := f.m.loadRun(j, q.Operation)
				if e != nil {
					t.Fatal(e)
				}
				v.ID = rec.DockerID
				v.State.Running = false
				v.State.StartedAt = ""
				out, e := f.m.runStatus(context.Background(), j, q.Operation)
				if e != nil || out.Phase != "unknown" {
					t.Fatal(out, e)
				}
				if _, e = f.m.executeRun(context.Background(), q, nil, j, &lifecycle.State{ID: j.HostID}, state); e != nil || starts != 1 {
					t.Fatal(e, starts)
				}
				delete(f.docker.containers, "vamos-run-"+q.Operation)
				out, err := f.m.runStatus(context.Background(), j, q.Operation)
				if err != nil || out.Phase != "unknown" {
					t.Fatal(out, err)
				}
				if f.m.preflightRuns(context.Background(), j) == nil {
					t.Fatal("unknown execution did not block")
				}
			}
		})
	}
}

func TestRunRecordStrictPrivateAndBounded(t *testing.T) {
	m, _ := setup(t)
	j := &journal{HostID: strings.Repeat("a", 32), Status: Status{App: "test"}}
	r := &runRecord{Version: 1, App: j.App, HostID: j.HostID, RunResult: RunResult{Operation: strings.Repeat("b", 32), Service: "worker", Phase: "building", Container: "vamos-run-" + strings.Repeat("b", 32)}, Config: config.Service{Image: "alpine:latest", Update: config.UpdateAuto, Command: []string{"true"}}}
	if err := m.saveRun(j, r); err != nil {
		t.Fatal(err)
	}
	p := runPath(m, j, r.Operation)
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range [][]byte{append(original, []byte(` {}`)...), []byte(strings.Replace(string(original), `"Version":1`, `"Version":1,"Version":1`, 1)), []byte(strings.Replace(string(original), `"Version":1`, `"Version":1,"Unexpected":true`, 1)), []byte(strings.Repeat("x", runRecordLimit+1))} {
		if err = os.WriteFile(p, body, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = m.loadRun(j, r.Operation); err == nil {
			t.Fatal("accepted malformed record")
		}
	}
	if err = os.WriteFile(p, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = m.loadRun(j, r.Operation); err == nil && runtime.GOOS != "windows" {
		t.Fatal("accepted public record")
	}
	r.Output = strings.Repeat("x", runOutputLimit+1)
	if m.saveRun(j, r) == nil {
		t.Fatal("accepted oversized output")
	}
}

func TestRunFollowupValidation(t *testing.T) {
	j := &journal{HostID: strings.Repeat("a", 32), Status: Status{App: "test"}}
	base := runRecord{Version: 1, App: j.App, HostID: j.HostID, RunResult: RunResult{Operation: strings.Repeat("b", 32), Service: "worker", Phase: "building", Container: "vamos-run-" + strings.Repeat("b", 32)}, Config: config.Service{Image: "alpine:latest", Update: config.UpdateAuto, Command: []string{"true"}}}
	if err := validateRun(j, &base); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*runRecord){
		"source":        func(r *runRecord) { r.Config.Image = "" },
		"empty command": func(r *runRecord) { r.Config.Command = []string{""} },
		"total argv": func(r *runRecord) {
			r.Config.Command = []string{strings.Repeat("a", 8192), strings.Repeat("a", 8192), strings.Repeat("a", 8192), strings.Repeat("a", 8192), "x"}
		},
		"building identity": func(r *runRecord) { r.DockerID = strings.Repeat("a", 64) },
		"exit bounds": func(r *runRecord) {
			r.Phase = "complete"
			r.Image = "sha256:" + strings.Repeat("a", 64)
			r.DockerID = strings.Repeat("a", 64)
			n := 256
			r.ExitCode = &n
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := base
			change(&r)
			if validateRun(j, &r) == nil {
				t.Fatal("accepted invalid record")
			}
		})
	}
	b, _ := json.Marshal(base.RunResult)
	if !strings.Contains(string(b), `"operation"`) {
		t.Fatal(string(b))
	}
}

func TestRunBuildingDoesNotBlock(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	r := &runRecord{Version: 1, App: j.App, HostID: j.HostID, RunResult: RunResult{Operation: strings.Repeat("b", 32), Service: "worker", Phase: "building", Container: "vamos-run-" + strings.Repeat("b", 32)}, Config: config.Service{Image: "alpine:latest", Update: config.UpdateAuto, Command: []string{"true"}}}
	if err := f.m.saveRun(j, r); err != nil {
		t.Fatal(err)
	}
	if err := f.m.preflightRuns(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	out, err := f.m.runStatus(context.Background(), j, r.Operation)
	if err != nil || out.Phase != "not-started" {
		t.Fatal(out, err)
	}
}

func TestRunCompletedCleanupAndLiveGuard(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	code := 7
	r := &runRecord{Version: 1, App: j.App, HostID: j.HostID, RunResult: RunResult{Operation: strings.Repeat("b", 32), Service: "worker", Phase: "complete", Container: "vamos-run-" + strings.Repeat("b", 32), Image: "sha256:" + strings.Repeat("a", 64), ExitCode: &code, Output: "durable output"}, DockerID: strings.Repeat("c", 64), Config: config.Service{Image: "alpine:latest", Update: config.UpdateAuto, Command: []string{"true"}}}
	if err := f.m.saveRun(j, r); err != nil {
		t.Fatal(err)
	}
	output, err := f.m.runLogs(context.Background(), j, r.Operation)
	if err != nil || output != r.Output {
		t.Fatal(output, err)
	}
	if err := f.m.preflightRuns(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	v := &inspection{ID: r.DockerID}
	v.State.Running = true
	f.docker.containers[r.Container] = v
	if f.m.preflightRuns(context.Background(), j) == nil {
		t.Fatal("live terminal container accepted")
	}
	out, err := f.m.runStatus(context.Background(), j, r.Operation)
	if err != nil || out.Phase != "complete" || *out.ExitCode != 7 {
		t.Fatal(out, err)
	}
}

func TestRunContextCollisionAndHostDrift(t *testing.T) {
	for _, kind := range []string{"context", "host"} {
		t.Run(kind, func(t *testing.T) {
			f := newMultiFlow(t)
			if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			j := f.load()
			q := f.req
			q.Operation = strings.Repeat("b", 32)
			if kind == "context" {
				if err := os.WriteFile(filepath.Join(f.m.dir(j.App), q.Operation+".context.tar"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(f.m.path("/etc/docker/daemon.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.m.executeRun(context.Background(), q, nil, j, &lifecycle.State{ID: j.HostID}, nil)
			if err == nil || (kind == "context" && !strings.Contains(err.Error(), "context provenance")) || (kind == "host" && !strings.Contains(err.Error(), "daemon configuration drift")) {
				t.Fatal(err)
			}
		})
	}
}
