package deploy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
)

func secretMultiApp() *config.App {
	a := multiApp()
	for name, s := range a.Services {
		s.Secrets = map[string]string{"TOKEN": "credential"}
		a.Services[name] = s
	}
	return a
}
func stageMultiSecret(t *testing.T, f *multiFlow, version, value string) {
	t.Helper()
	if _, err := f.m.secretStore().Stage(f.req.App, strings.Repeat("a", 32), "credential", version, []byte(value)); err != nil {
		t.Fatal(err)
	}
}
func TestMultiSecretPinsSurvivePartialProgressAndNewStage(t *testing.T) {
	f := newMultiFlow(t)
	a, b, c := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	stageMultiSecret(t, f, a, "synthetic-value-A")
	if _, err := f.deploy(secretMultiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	stageMultiSecret(t, f, b, "synthetic-value-B")
	f.docker.beforeRun = func(args []string) {
		if strings.Contains(fakeArg(args, "--name"), "-backend-") {
			f.docker.lose["run"] = 1
		}
	}
	if _, err := f.deploy(secretMultiApp(), strings.Repeat("2", 32)); err == nil {
		t.Fatal("expected partial operation")
	}
	j := f.load()
	state, err := f.m.secretStore().Load(j.App, j.HostID)
	if err != nil {
		t.Fatal(err)
	}
	status := f.m.multiStatus(context.Background(), j, state, false, "")
	if len(status.Secrets) != 1 || status.Secrets[0].AppliedVersion != "" || status.Secrets[0].AppliedVersions["web"] != b || status.Secrets[0].AppliedVersions["backend"] != a || !status.Secrets[0].Pending {
		t.Fatal(status.Secrets)
	}
	stageMultiSecret(t, f, c, "synthetic-value-C")
	f.docker.beforeRun = nil
	if _, err = f.resume(j.Operation); err != nil {
		t.Fatal(err)
	}
	j = f.load()
	state, err = f.m.secretStore().Load(j.App, j.HostID)
	if err != nil {
		t.Fatal(err)
	}
	status = f.m.multiStatus(context.Background(), j, state, false, "")
	if status.Secrets[0].AppliedVersion != b || !status.Secrets[0].Pending {
		t.Fatal("resume substituted latest", status.Secrets)
	}
	data, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "synthetic-value-") {
		t.Fatal("value in journal")
	}
}
func TestMultiSelectedSecretStatusShowsOnlySelectedPins(t *testing.T) {
	f := newMultiFlow(t)
	stageMultiSecret(t, f, strings.Repeat("a", 32), "synthetic-value-A")
	if _, err := f.deploy(secretMultiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	state, err := f.m.secretStore().Load(j.App, j.HostID)
	if err != nil {
		t.Fatal(err)
	}
	status := f.m.multiStatus(context.Background(), j, state, false, "backend")
	if len(status.Services) != 1 || len(status.Secrets) != 1 || len(status.Secrets[0].Services) != 1 || status.Secrets[0].Services[0] != "backend" || len(status.Secrets[0].AppliedVersions) != 1 {
		t.Fatal("selected status retained unrelated service metadata", status.Secrets)
	}
}
func TestMultiPausedServiceDoesNotClaimAvailability(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	f.docker.containers[j.ServiceStates["backend"].Current.Container].State.Paused = true
	status := f.m.multiStatus(context.Background(), j, nil, false, "")
	if status.Services["backend"].Available {
		t.Fatal("paused process reported available")
	}
}

func TestMultiDestroyingStatusDoesNotClaimAvailability(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	j.Phase = "destroying"
	status := f.m.multiStatus(context.Background(), j, nil, false, "")
	for name, s := range status.Services {
		if s.Available {
			t.Fatal(name, "claimed available after route withdrawal intent")
		}
	}
}
