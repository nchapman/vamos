package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBackupPlanRuntime(t *testing.T) {
	for _, scenario := range []string{"fresh", "effective", "stopped", "missing", "foreign", "restarting", "legacy"} {
		t.Run(scenario, func(t *testing.T) {
			f := newMultiFlow(t)
			if scenario == "fresh" {
				f.req.Partial = true
			}
			if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			j := f.load()
			// Explicit fixture upgrade, never an implicit production migration.
			upgradeServiceJournal(j)
			var name string
			for n := range j.ServiceStates {
				name = n
				break
			}
			slot := j.ServiceStates[name]
			switch scenario {
			case "stopped":
				f.docker.containers[slot.Current.Container].State.Running = false
			case "missing":
				delete(f.docker.containers, slot.Current.Container)
			case "foreign":
				f.docker.containers[slot.Current.Container].Config.Labels["vamos.host"] = "foreign"
			case "restarting":
				f.docker.containers[slot.Current.Container].State.Restarting = true
			}
			if err := f.m.save(j); err != nil {
				t.Fatal(err)
			}
			// Exercise the projection's defensive independence from desired input;
			// this altered view is not a valid persisted ready journal.
			if scenario == "effective" {
				cfg := j.Config.Services[name]
				cfg.Env = map[string]string{"DIFFERENT": "desired"}
				j.Config.Services[name] = cfg
			}
			path := filepath.Join(f.m.dir(j.App), "state.json")
			disk, _ := os.ReadFile(path)
			before, _ := json.Marshal(j)
			events := append([]string(nil), f.docker.events...)
			if scenario == "legacy" {
				for v := 1; v <= 6; v++ {
					j.Version = v
					if _, err := f.m.backupPlanRuntime(context.Background(), j); err == nil || !strings.Contains(err.Error(), "current identity-aware deployment") {
						t.Fatal(err)
					}
				}
				j.Version = 7
			} else {
				got, err := f.m.backupPlanRuntime(context.Background(), j)
				wantOK := scenario == "fresh" || scenario == "effective" || scenario == "stopped"
				if (err == nil) != wantOK {
					t.Fatalf("result: %+v, %v", got, err)
				}
				if wantOK && (got.Services[name].Incarnation != slot.Incarnation || got.Services[name].Replicas != slot.Current.Config.ReplicaCount()) {
					t.Fatal(got)
				}
			}
			after, _ := json.Marshal(j)
			diskAfter, _ := os.ReadFile(path)
			if string(before) != string(after) || string(disk) != string(diskAfter) {
				t.Fatal("journal mutated")
			}
			if !reflect.DeepEqual(events, f.docker.events) {
				t.Fatalf("mutation events: %v", f.docker.events[len(events):])
			}
		})
	}
}
