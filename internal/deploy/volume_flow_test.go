package deploy

import (
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
)

func volumeFlowApp(revision string) *config.App {
	a := multiApp()
	for name, s := range a.Services {
		s.Volume = &config.Volume{Name: name + "-data", Mount: "/data, persistent"}
		s.Env = map[string]string{"REVISION": revision}
		a.Services[name] = s
	}
	return a
}

func TestVolumeFlowInitialChangedAndUnchanged(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(volumeFlowApp("one"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if j.Version != 3 || j.Phase != "ready" {
		t.Fatalf("unexpected state: %+v", j.Status)
	}
	ids := map[string]string{}
	old := map[string]string{}
	for name, slot := range j.ServiceStates {
		ids[name] = slot.VolumeID
		if slot.VolumeID == "" {
			t.Fatal("missing durable volume identity")
		}
		old[name] = slot.Current.Container
	}
	if eventCount(f.docker, "run") != 0 || eventCount(f.docker, "create") != 2 {
		t.Fatal(f.docker.events)
	}
	starts := eventCount(f.docker, "start")
	if _, err := f.deploy(volumeFlowApp("one"), strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	if eventCount(f.docker, "start") != starts {
		t.Fatal("unchanged deployment restarted writer")
	}
	if _, err := f.deploy(volumeFlowApp("two"), strings.Repeat("3", 32)); err != nil {
		t.Fatal(err)
	}
	for name, slot := range f.load().ServiceStates {
		if slot.VolumeID != ids[name] {
			t.Fatal("volume identity changed")
		}
		if v := f.docker.containers[old[name]]; v != nil && (v.State.Running || v.HostConfig.RestartPolicy.Name != "no") {
			t.Fatal("old writer not retired")
		}
	}
}

func TestVolumeFlowLostMutationResponses(t *testing.T) {
	for _, key := range []string{"volume-create", "create", "update", "start", "stop"} {
		t.Run(key, func(t *testing.T) {
			f := newMultiFlow(t)
			if key == "stop" {
				if _, err := f.deploy(volumeFlowApp("one"), strings.Repeat("1", 32)); err != nil {
					t.Fatal(err)
				}
			}
			f.docker.lose[key] = 1
			if _, err := f.deploy(volumeFlowApp("two"), strings.Repeat("2", 32)); err != nil {
				if _, retryErr := f.resume(strings.Repeat("2", 32)); retryErr != nil {
					t.Fatalf("lost %s: initial=%v; resume=%v", key, err, retryErr)
				}
			}
			if f.docker.lose[key] != 0 || f.load().Phase != "ready" {
				t.Fatalf("mutation not recovered: %s", key)
			}
		})
	}
}

func TestVolumeFlowMissingOrReplacedFailsBeforeExecution(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		name := "missing"
		if replaced {
			name = "replaced"
		}
		t.Run(name, func(t *testing.T) {
			f := newMultiFlow(t)
			if _, err := f.deploy(volumeFlowApp("one"), strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			for n, v := range f.docker.volumes {
				if replaced {
					v.CreatedAt = "2026-06-02T00:00:00Z"
				} else {
					delete(f.docker.volumes, n)
				}
				break
			}
			f.docker.events = nil
			if _, err := f.deploy(volumeFlowApp("two"), strings.Repeat("2", 32)); err == nil {
				t.Fatal("accepted missing/replaced volume")
			}
			for _, key := range []string{"create", "start", "stop", "update", "run"} {
				if eventCount(f.docker, key) != 0 {
					t.Fatalf("unsafe mutation %s: %v", key, f.docker.events)
				}
			}
		})
	}
}
