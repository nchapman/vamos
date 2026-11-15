package deploy

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/process"
)

func TestMaintenanceAllowsOtherAppWithSharedNamePrefix(t *testing.T) {
	m := NewManager()
	m.Root = t.TempDir()
	j := &journal{HostID: strings.Repeat("a", 32), Status: Status{App: "notes", Operation: strings.Repeat("b", 32)}}
	if err := os.MkdirAll(m.dir(j.App), 0700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 64)
	name := "vamos-notes-worker-" + strings.Repeat("d", 32)
	m.Run = func(_ context.Context, _ string, args []string, _ []byte) (process.Result, error) {
		var value any
		switch {
		case args[0] == "container" && args[1] == "ls":
			value = resourceRow{ID: id, Names: name}
		case args[0] == "container" && args[1] == "inspect":
			v := resourceInspection{Name: "/" + name, Image: "sha256:" + strings.Repeat("e", 64)}
			v.ID = id
			v.Config.Labels = map[string]string{"vamos.host": j.HostID, "vamos.app": "notes-worker"}
			value = []resourceInspection{v}
		case args[1] == "ls":
			return process.Result{}, nil
		default:
			t.Fatalf("unexpected command %v", args)
		}
		b, err := json.Marshal(value)
		return process.Result{Stdout: b}, err
	}
	if _, err := m.planMaintenance(context.Background(), j, "cleanup", strings.Repeat("f", 32)); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceAllowsHostBuildKitContainerForBuildKitApp(t *testing.T) {
	m := NewManager()
	m.Root = t.TempDir()
	hostID := strings.Repeat("a", 32)
	j := &journal{HostID: hostID, Status: Status{App: "buildkit", Operation: strings.Repeat("b", 32)}}
	if err := os.MkdirAll(m.dir(j.App), 0700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 64)
	name := "vamos-buildkit-" + hostID
	m.Run = func(_ context.Context, _ string, args []string, _ []byte) (process.Result, error) {
		var value any
		switch {
		case args[0] == "container" && args[1] == "ls":
			value = resourceRow{ID: id, Names: name}
		case args[0] == "container" && args[1] == "inspect":
			v := resourceInspection{Name: "/" + name, Image: "sha256:" + strings.Repeat("e", 64)}
			v.ID = id
			v.Config.Labels = map[string]string{"org.vamos.buildkit.host": hostID, "org.vamos.buildkit.role": "builder"}
			value = []resourceInspection{v}
		case args[1] == "ls":
			return process.Result{}, nil
		default:
			t.Fatalf("unexpected command %v", args)
		}
		b, err := json.Marshal(value)
		return process.Result{Stdout: b}, err
	}
	if _, err := m.planMaintenance(context.Background(), j, "cleanup", strings.Repeat("f", 32)); err != nil {
		t.Fatal(err)
	}
}
