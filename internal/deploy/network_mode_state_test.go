package deploy

import (
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
)

func TestNetworkModeReleaseHistory(t *testing.T) {
	f := newMultiFlow(t)
	a := multiApp()
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	s := a.Services["backend"]
	s.NetworkMode = "host"
	a.Services["backend"] = s
	if _, err := f.deploy(a, strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if j.Completed.Services["backend"].Config.NetworkMode != "host" || j.PreviousCompleted.Services["backend"].Config.NetworkMode != "" {
		t.Fatal("mode-only change not captured in completed snapshots")
	}
	for _, tc := range []struct{ op, want string }{{"3", ""}, {"4", "host"}} {
		if _, err := f.m.Execute(t.Context(), rollbackRequest(f, tc.op, "", true), nil); err != nil {
			t.Fatal(err)
		}
		j = f.load()
		if j.Config.Services["backend"].NetworkMode != tc.want || j.ServiceStates["backend"].Current.Config.NetworkMode != tc.want {
			t.Fatalf("rollback did not restore mode %q", tc.want)
		}
	}
}

func TestMaintenanceBasisBindsCapturedNetworkMode(t *testing.T) {
	cfg := config.Service{}
	j := &journal{PreviousCompleted: &completedRelease{Services: map[string]*release{"backend": {Config: &cfg}}}}
	before, err := maintenanceBasis(j)
	if err != nil {
		t.Fatal(err)
	}
	cfg.NetworkMode = "host"
	after, err := maintenanceBasis(j)
	if err != nil || after == before {
		t.Fatalf("maintenance basis ignored captured host mode: %v", err)
	}
}
