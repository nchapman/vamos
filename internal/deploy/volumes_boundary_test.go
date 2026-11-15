package deploy

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/process"
	"github.com/nchapman/vamos/internal/volumes"
)

func TestVolumeMountCSVBoundary(t *testing.T) {
	name, mount := "vamos-vol-abc", `/data,with"quotes`
	fields, err := csv.NewReader(strings.NewReader(volumeMountArgument(name, mount))).Read()
	if err != nil || !reflect.DeepEqual(fields, []string{"type=volume", "source=" + name, "target=" + mount, "volume-nocopy"}) {
		t.Fatalf("fields %q: %v", fields, err)
	}
}
func volumeBoundaryFixture(t *testing.T) (*Manager, *journal, volumes.Record) {
	t.Helper()
	m, _ := setup(t)
	j := &journal{Version: 3, HostID: strings.Repeat("a", 32), Status: Status{App: "notes", Operation: strings.Repeat("b", 32)}, Config: &config.App{Services: map[string]config.Service{"web": {Volume: &config.Volume{Name: "data", Mount: "/data"}}}}, ServiceStates: map[string]*serviceState{"web": {VolumeID: strings.Repeat("c", 32)}}}
	rec := volumes.Record{ID: strings.Repeat("c", 32), Service: "web", Name: "data", Mount: "/data", Operation: j.Operation, State: "planned"}
	if err := m.volumeStore().Save(&volumes.State{Version: 1, App: j.App, HostID: j.HostID, Records: map[string]volumes.Record{"web": rec}}); err != nil {
		t.Fatal(err)
	}
	return m, j, rec
}
func TestVolumeLostCreateResponse(t *testing.T) {
	m, j, rec := volumeBoundaryFixture(t)
	created := false
	calls := []string{}
	m.Run = func(_ context.Context, n string, a []string, _ []byte) (process.Result, error) {
		cmd := strings.Join(a, " ")
		calls = append(calls, cmd)
		var data []byte
		switch {
		case cmd == "volume ls --format {{json .}}":
			if created {
				data = []byte(`{"Name":"` + rec.DockerName() + `"}`)
			}
		case cmd == "container ls --all --filter volume="+rec.DockerName()+" --format {{json .}}":
		case strings.HasPrefix(cmd, "volume create --driver local "):
			want := []string{"volume", "create", "--driver", "local"}
			for _, key := range []string{"vamos.host", "vamos.app", "vamos.service", "vamos.volume", "vamos.operation"} {
				want = append(want, "--label", key+"="+volumeLabels(j, rec)[key])
			}
			want = append(want, rec.DockerName())
			if !reflect.DeepEqual(a, want) {
				t.Fatalf("unsafe create: %v", a)
			}
			created = true
			return process.Result{}, errors.New("lost response")
		case cmd == "volume inspect "+rec.DockerName():
			data, _ = json.Marshal([]any{map[string]any{"Name": rec.DockerName(), "Driver": "local", "Scope": "local", "Options": map[string]string{}, "Labels": volumeLabels(j, rec), "CreatedAt": "2026-01-01T00:00:00Z", "Mountpoint": "/ignored/host/path"}})
		default:
			t.Fatalf("unexpected command %s %s", n, cmd)
		}
		return process.Result{Stdout: data}, nil
	}
	if err := m.ensureVolumes(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	s, err := m.volumeStore().Load(j.App, j.HostID)
	if err != nil || s.Records["web"].State != "created" {
		t.Fatalf("intent not persisted: %v %v", s, err)
	}
	created = false
	calls = nil
	if err = m.ensureVolumes(context.Background(), j); err == nil {
		t.Fatal("missing created volume accepted")
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "volume create") {
			t.Fatal("recreated lost data")
		}
	}
}
func TestVolumePlanImmutableAndReuse(t *testing.T) {
	m, j, rec := volumeBoundaryFixture(t)
	// A failed first deployment still reserves its service binding.
	j.Config.Services["web"] = config.Service{}
	if _, err := m.planVolumes(Request{}, nil, j); err == nil {
		t.Fatal("stateless reuse accepted")
	}
	j.Config.Services["web"] = config.Service{Volume: &config.Volume{Name: "data", Mount: "/data"}}
	rec.State = "created"
	rec.CreatedAt = "2026-01-01T00:00:00Z"
	if err := m.volumeStore().Save(&volumes.State{Version: 1, App: j.App, HostID: j.HostID, Records: map[string]volumes.Record{"web": rec}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.planVolumes(Request{}, nil, j); err == nil {
		t.Fatal("unacknowledged retained reuse")
	}
	if _, err := m.planVolumes(Request{ReuseData: true}, nil, j); err != nil {
		t.Fatal(err)
	}
}
func TestVolumeRuntimeMountBoundaries(t *testing.T) {
	_, _, r := volumeBoundaryFixture(t)
	good := `{"Mounts":[{"Type":"volume","Name":"` + r.DockerName() + `","Source":"/host/path","Destination":"/data","Driver":"local","RW":true}],"HostConfig":{"Mounts":[{"Type":"volume","Source":"` + r.DockerName() + `","Target":"/data","VolumeOptions":{"NoCopy":true,"DriverConfig":{}}}]}}`
	for _, tc := range []struct{ name, from, to string }{{"valid", "", ""}, {"copy", "\"NoCopy\":true", "\"NoCopy\":false"}, {"readonly", "\"RW\":true", "\"RW\":false"}, {"subpath", "\"NoCopy\":true", "\"NoCopy\":true,\"Subpath\":\"other\""}, {"driver options", "\"DriverConfig\":{}", "\"DriverConfig\":{\"Name\":\"local\"}"}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := good
			if tc.from != "" {
				raw = strings.ReplaceAll(raw, tc.from, tc.to)
			}
			var v inspection
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				t.Fatal(err)
			}
			err := validateVolumeMounts(&v, r)
			if (err == nil) != (tc.name == "valid") {
				t.Fatalf("unexpected validation: %v", err)
			}
		})
	}
}
