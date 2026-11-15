package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/process"
)

func TestVolumeSupersedeCancelsOnlyUnservedActivationFrom503(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(volumeFlowApp("old"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	run := f.m.Run
	reloads := 0
	f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
		if name == "caddy" && args[0] == "reload" {
			reloads++
			if reloads == 2 {
				return process.Result{}, errors.New("activation not applied")
			}
		}
		return run(ctx, name, args, input)
	}
	failed := strings.Repeat("2", 32)
	if _, err := f.deploy(volumeFlowApp("next"), failed); err == nil {
		t.Fatal("expected failed activation")
	}
	j := f.load()
	slot := j.ServiceStates["web"]
	id := slot.VolumeID
	if j.NewCaddy == "" || !slot.Unavailable || !slot.CurrentRetired || !strings.Contains(j.OldCaddy, `respond "Service unavailable" 503`) {
		t.Fatal("activation did not retain503 baseline")
	}
	f.m.Run = run
	q := f.req
	q.Operation = strings.Repeat("3", 32)
	q.Config = volumeFlowApp("fixed")
	q.Supersede = failed
	if _, err := f.m.Execute(context.Background(), q, f.bundle(q.Config)); err != nil {
		t.Fatal(err)
	}
	current := f.load().ServiceStates["web"]
	if current.VolumeID != id || current.Unavailable || f.docker.containers[slot.Current.Container].State.Running {
		t.Fatal("cancelled activation resurrected old writer or lost storage")
	}
}
