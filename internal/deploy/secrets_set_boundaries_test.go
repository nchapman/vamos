package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/secrets"
)

func TestSecretBatchSetReplacesEachConsumerOnce(t *testing.T) {
	f := newMultiFlow(t)
	stageMultiSecret(t, f, strings.Repeat("a", 32), "first")
	if _, err := f.m.secretStore().Stage(f.req.App, strings.Repeat("a", 32), "other", strings.Repeat("b", 32), []byte("second")); err != nil {
		t.Fatal(err)
	}
	a := secretMultiApp()
	for name, s := range a.Services {
		s.Secrets["OTHER"] = "other"
		a.Services[name] = s
	}
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	before := len(f.docker.events)
	r := Request{Version: 1, Action: "secrets-set", App: f.req.App, Host: f.req.Host, Operation: strings.Repeat("2", 32), SecretNames: []string{"credential", "other"}}
	input, err := secrets.EncodeBatch(map[string][]byte{"credential": []byte("new-first"), "other": []byte("new-second")})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.m.Execute(context.Background(), r, input)
	if err != nil || !resp.SetApplied || len(resp.SecretVersions) != 2 {
		t.Fatal(resp, err)
	}
	runs := 0
	for _, event := range f.docker.events[before:] {
		if event == "run" {
			runs++
		}
	}
	if runs != 2 {
		t.Fatalf("%d replacements for two consumers", runs)
	}
	for _, s := range f.load().ServiceStates {
		for _, ref := range s.Current.SecretRefs {
			if ref.Version != secrets.VersionFor(r.Operation, ref.Name) {
				t.Fatal("batch pin mismatch")
			}
		}
	}
}

func TestSecretSetRefusalsDoNotStageOrReplace(t *testing.T) {
	for _, kind := range []string{"manual", "stopped", "missing-image", "drift", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			f := newMultiFlow(t)
			stageMultiSecret(t, f, strings.Repeat("a", 32), "original")
			a := secretMultiApp()
			if kind == "manual" {
				s := a.Services["backend"]
				s.Update = config.UpdateManual
				a.Services["backend"] = s
			}
			if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			j := f.load()
			switch kind {
			case "stopped":
				f.docker.containers[j.ServiceStates["backend"].Current.Container].State.Running = false
			case "missing-image":
				delete(f.docker.images, j.ServiceStates["backend"].Current.Image)
			case "drift":
				f.docker.containers[j.ServiceStates["backend"].Current.Container].Config.Labels["soar.host"] = "foreign"
			case "incomplete":
				f.failHealth["web"] = true
				if _, err := f.deploy(changedMultiApp("pending"), strings.Repeat("3", 32)); err == nil {
					t.Fatal("expected failed deployment")
				}
			}
			path := filepath.Join(f.m.secretStore().Dir, f.req.App+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			events := len(f.docker.events)
			if _, err := setSecret(t, f, "credential", strings.Repeat("2", 32), "replacement"); err == nil {
				t.Fatal("unsafe set allowed")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("refusal staged a value", err)
			}
			for _, e := range f.docker.events[events:] {
				if e == "stop" || e == "run" || e == "build" || e == "pull" {
					t.Fatal("refusal mutated runtime", e)
				}
			}
		})
	}
}

func TestSecretSetKeepsPerServiceImagesForSharedSource(t *testing.T) {
	f := newMultiFlow(t)
	stageMultiSecret(t, f, strings.Repeat("a", 32), "old")
	a := secretMultiApp()
	b := a.Services["backend"]
	b.Build = a.Services["web"].Build
	a.Services["backend"] = b
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	f.docker.buildHook = func([]string, []byte) string { return "sha256:" + strings.Repeat("c", 64) }
	f.req.Service = "web"
	if _, err := f.deploy(a, strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	before := f.load()
	if before.ServiceStates["web"].Current.Image == before.ServiceStates["backend"].Current.Image {
		t.Fatal("fixture requires divergent images")
	}
	if _, err := setSecret(t, f, "credential", strings.Repeat("3", 32), "new"); err != nil {
		t.Fatal(err)
	}
	for name, s := range f.load().ServiceStates {
		if s.Current.Image != before.ServiceStates[name].Current.Image {
			t.Fatal("coalesced distinct images")
		}
	}
}

func TestSecretSetDoesNotRerunReleaseCommand(t *testing.T) {
	f := newReleaseFlow(t)
	stageMultiSecret(t, f.multiFlow, strings.Repeat("a", 32), "old")
	a := secretMultiApp()
	a.Release = &config.Release{Service: "web", Command: []string{"bin/release"}}
	if _, err := f.deploy(a, strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	starts, builds := f.starts, f.builds
	if _, err := setSecret(t, f.multiFlow, "credential", strings.Repeat("2", 32), "new"); err != nil {
		t.Fatal(err)
	}
	if f.starts != starts || f.builds != builds || f.load().Hook != nil {
		t.Fatal("set ran a release command or build")
	}
}

func TestSecretSetUnusedStagesWithoutCreatingDeployment(t *testing.T) {
	f := newMultiFlow(t)
	resp, err := setSecret(t, f, "unused", strings.Repeat("1", 32), "value")
	if err != nil || !resp.SetApplied || resp.Secret == nil || resp.Status != nil {
		t.Fatal(resp, err)
	}
	if j, err := f.m.load(f.req.App); j != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("set started an undeployed app", err)
	}
	if len(f.docker.containers) != 0 || f.builds != 0 {
		t.Fatal("unused secret started resources")
	}
}

func TestSecretUpdateCaptureRejectsMalformedInputs(t *testing.T) {
	f := newMultiFlow(t)
	stageMultiSecret(t, f, strings.Repeat("a", 32), "old")
	if _, err := f.deploy(secretMultiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	r := Request{Operation: strings.Repeat("2", 32), SecretName: "credential"}
	planned, err := secretUpdatePlan(r, f.load())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(planned)
	for name, change := range map[string]func(*journal){
		"legacy version": func(j *journal) { j.Version = 7 },
		"source archive": func(j *journal) { j.Archive = j.Operation + ".context.tar" },
		"build result":   func(j *journal) { j.BuildImages = map[string]string{"x": "sha256:" + strings.Repeat("a", 64)} },
		"missing image":  func(j *journal) { delete(j.SecretUpdate.Images, "web") },
		"extra image":    func(j *journal) { j.SecretUpdate.Images["unknown"] = "sha256:" + strings.Repeat("a", 64) },
		"invalid image":  func(j *journal) { j.SecretUpdate.Images["web"] = "latest" },
		"wrong pin":      func(j *journal) { j.SecretUpdate.Versions["credential"] = strings.Repeat("b", 32) },
		"manual consumer": func(j *journal) {
			s := j.Config.Services["web"]
			s.Update = config.UpdateManual
			j.Config.Services["web"] = s
		},
		"missing selection": func(j *journal) { j.Selected = nil },
		"release directive": func(j *journal) {
			j.Config.Release = &config.Release{Service: "web", Command: []string{"touch", "/sentinel"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var j journal
			if err := json.Unmarshal(data, &j); err != nil {
				t.Fatal(err)
			}
			change(&j)
			if validateMultiJournal(&j, j.App) == nil {
				t.Fatal("accepted malformed capture")
			}
		})
	}
}

func TestSecretSetCompletionSupportsRollbackAndRemoval(t *testing.T) {
	f := newMultiFlow(t)
	stageMultiSecret(t, f, strings.Repeat("a", 32), "old")
	if _, err := f.deploy(secretMultiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	old := f.load()
	if _, err := setSecret(t, f, "credential", strings.Repeat("2", 32), "new"); err != nil {
		t.Fatal(err)
	}
	r := Request{Version: 1, Action: "rollback", App: f.req.App, Host: f.req.Host, Operation: strings.Repeat("3", 32), Apply: true}
	if _, err := f.m.Execute(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	for name, s := range f.load().ServiceStates {
		if !maps.Equal(s.Current.SecretRefs, old.ServiceStates[name].Current.SecretRefs) {
			t.Fatal("rollback did not restore pins")
		}
	}
	if _, err := setSecret(t, f, "credential", strings.Repeat("4", 32), "new-again"); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	j.Maintenance = &maintenance{Service: "web", Action: "service-remove", Operation: strings.Repeat("5", 32)}
	commitServiceRemoval(j)
	if err := validateMultiJournal(j, j.App); err != nil {
		t.Fatal(err)
	}
	if _, ok := j.SecretUpdate.Images["web"]; ok {
		t.Fatal("removed consumer still captured")
	}
	if !reflect.DeepEqual(j.Selected, []string{"backend"}) {
		t.Fatal(j.Selected)
	}
}
