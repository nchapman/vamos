package deploy

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
)

func setSecret(t *testing.T, f *multiFlow, name, op, value string) (Response, error) {
	t.Helper()
	r := Request{Version: 1, Action: "secrets-set", App: f.req.App, Host: f.req.Host, Operation: op, SecretName: name}
	return f.m.Execute(context.Background(), r, []byte(value))
}

func TestSecretSetReusesDeployedImagesAndOnlyChangesSelectedPins(t *testing.T) {
	f := newMultiFlow(t)
	a := secretMultiApp()
	a.Services["unrelated"] = config.Service{Build: &config.Build{Context: ".", Dockerfile: "Unrelatedfile"}, Update: config.UpdateAuto}
	for name, s := range a.Services {
		if name != "unrelated" {
			s.Secrets["OTHER"] = "other"
			a.Services[name] = s
		}
	}
	stageMultiSecret(t, f, strings.Repeat("a", 32), "old-token")
	for _, v := range []struct{ op, value string }{{strings.Repeat("b", 32), "old-other"}} {
		if _, err := f.m.secretStore().Stage(f.req.App, strings.Repeat("a", 32), "other", v.op, []byte(v.value)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	before := f.load()
	if _, err := f.m.secretStore().Stage(f.req.App, before.HostID, "other", strings.Repeat("c", 32), []byte("pending-other")); err != nil {
		t.Fatal(err)
	}
	builds := f.builds
	events := len(f.docker.events)
	resp, err := setSecret(t, f, "credential", strings.Repeat("2", 32), "new-token")
	if err != nil {
		t.Fatalf("set failed: %v (%+v)", err, resp)
	}
	after := f.load()
	if after.Phase != "ready" || f.builds != builds {
		t.Fatal("set built an image or did not finish")
	}
	runs := 0
	for _, e := range f.docker.events[events:] {
		if e == "run" {
			runs++
		}
		if e == "pull" || e == "build" {
			t.Fatal("set resolved an image")
		}
	}
	if runs != 2 {
		t.Fatalf("replaced %d containers, want exactly two consumers", runs)
	}
	for name, s := range after.ServiceStates {
		prior := before.ServiceStates[name].Current
		if s.Current.Image != prior.Image {
			t.Fatal("image changed", name)
		}
		if name == "unrelated" {
			if s.Current.Container != prior.Container {
				t.Fatal("replaced nonconsumer")
			}
			continue
		}
		if s.Current.SecretRefs["TOKEN"].Version != strings.Repeat("2", 32) || s.Current.SecretRefs["OTHER"] != prior.SecretRefs["OTHER"] {
			t.Fatal("wrong pins", s.Current.SecretRefs)
		}
		if !maps.Equal(s.Current.Config.Env, prior.Config.Env) {
			t.Fatal("config changed")
		}
	}
}

func TestSecretSetRecoveryKeepsCapturedVersion(t *testing.T) {
	f := newMultiFlow(t)
	stageMultiSecret(t, f, strings.Repeat("a", 32), "old")
	if _, err := f.deploy(secretMultiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	f.failHealth["web"] = true
	if _, err := setSecret(t, f, "credential", strings.Repeat("2", 32), "captured"); err == nil {
		t.Fatal("expected readiness failure")
	}
	stageMultiSecret(t, f, strings.Repeat("b", 32), "newer")
	f.failHealth["web"] = false
	if _, err := f.resume(strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.load().ServiceStates {
		if s.Current.SecretRefs["TOKEN"].Version != strings.Repeat("2", 32) {
			t.Fatal("resume repinned")
		}
	}
}
