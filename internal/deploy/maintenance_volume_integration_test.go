package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/process"
	"github.com/nchapman/vamos/internal/volumes"
)

func TestRetainedVolumeOnlyStatusAndMaintenance(t *testing.T) {
	for _, receipt := range []bool{false, true} {
		for _, foreign := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "absent", true: "destroyed"}[receipt], map[bool]string{false: "owned", true: "foreign"}[foreign]}, "/"), func(t *testing.T) {
				m, r := setup(t)
				j := &journal{Version: 1, HostID: strings.Repeat("a", 32), Status: Status{App: r.App, Phase: "destroyed", Operation: strings.Repeat("d", 32)}}
				rec := volumes.Record{ID: strings.Repeat("c", 32), Service: "db", Name: "data", Mount: "/data", Operation: strings.Repeat("b", 32), State: "created", CreatedAt: "2026-01-01T00:00:00Z"}
				if err := m.volumeStore().Save(&volumes.State{Version: 1, App: r.App, HostID: j.HostID, Records: map[string]volumes.Record{"db": rec}}); err != nil {
					t.Fatal(err)
				}
				if receipt {
					b, _ := json.Marshal(map[string]any{"version": 1, "app": j.App, "operation": j.Operation, "phase": "destroyed", "HostID": j.HostID})
					if err := atomic(filepath.Join(m.dir(j.App), "state.json"), b, 0600); err != nil {
						t.Fatal(err)
					}
				}
				m.Run = func(_ context.Context, _ string, a []string, _ []byte) (process.Result, error) {
					var b []byte
					switch {
					case a[0] == "volume" && a[1] == "ls":
						b = []byte(`{"Name":"` + rec.DockerName() + `"}`)
					case a[0] == "volume" && a[1] == "inspect":
						b, _ = json.Marshal([]any{map[string]any{"Name": rec.DockerName(), "Driver": "local", "Scope": "local", "Labels": volumeLabels(j, rec), "CreatedAt": rec.CreatedAt}})
					case a[0] == "container" && a[1] == "ls":
						if foreign {
							b = []byte(`{"Names":"foreign"}`)
						}
					case a[0] == "container" && a[1] == "inspect":
						b = []byte(`[{"Name":"/foreign"}]`)
					default:
						t.Fatalf("unexpected mutation %v", a)
					}
					return process.Result{Stdout: b}, nil
				}
				q := r
				q.Action = "status"
				q.Config = nil
				q.Operation = ""
				q.Service = "db"
				resp, err := m.Execute(context.Background(), q, nil)
				if err != nil || resp.Status == nil || len(resp.Status.Volumes) != 1 || resp.Status.Volumes[0].State != "retained" || !resp.Status.Volumes[0].DataMayHaveChanged {
					t.Fatalf("status %#v %v", resp, err)
				}
				for _, action := range []string{"cleanup", "destroy"} {
					q.Action = action
					q.Service = ""
					q.Operation = strings.Repeat("e", 32)
					q.Apply = true
					resp, err = m.Execute(context.Background(), q, nil)
					if foreign {
						if err == nil {
							t.Fatal("foreign reference accepted")
						}
					} else if err != nil || resp.Plan == nil || len(resp.Plan.Retained) != 1 {
						t.Fatalf("maintenance %#v %v", resp, err)
					}
					state, e := m.volumeStore().Load(r.App, j.HostID)
					if e != nil || state.Records["db"] != rec {
						t.Fatal("registry changed", e)
					}
				}
				m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
					return process.Result{}, errors.New("private-canary")
				}
				out := Response{Status: &Status{App: r.App}}
				m.enrichVolumeResponse(context.Background(), q, j.HostID, &out)
				if strings.Contains(out.Status.LastError, "private-canary") || !strings.Contains(out.Status.LastError, "volume metadata inspection failed") {
					t.Fatal(err, out)
				}
			})
		}
	}
}
