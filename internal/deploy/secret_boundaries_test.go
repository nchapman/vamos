package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/process"
)

func boundaryStore(t *testing.T, m *Manager, app string) []byte {
	t.Helper()
	b, err := os.ReadFile(m.path("/var/lib/soar/secrets/" + app + ".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSecretBoundaryStageFences(t *testing.T) {
	for _, kind := range []string{"host-lock", "global-maintenance"} {
		t.Run(kind, func(t *testing.T) {
			f, r := flowSetup(t)
			stageSecret(t, f.m, r, "1", secretCanary)
			if kind == "host-lock" {
				lock := flock.New(f.m.path(lifecycle.LockPath))
				ok, err := lock.TryLock()
				if err != nil || !ok {
					t.Fatalf("acquire test lock: %v, %v", ok, err)
				}
				defer lock.Close()
			} else {
				j := f.execute(r, "archive", false)
				j.Phase = "cleaning"
				j.Maintenance = &maintenance{Operation: strings.Repeat("e", 32), Action: "cleanup", Plan: Plan{Action: "cleanup", App: j.App}}
				if err := f.m.save(j); err != nil {
					t.Fatal(err)
				}
				r.App = "other"
				// The fence is host-wide, including apps without deployment journals.
			}
			before := boundaryStore(t, f.m, "notes")
			f.events = nil
			r.Action = "secrets-stage"
			r.Config = nil
			r.SecretName = "token"
			r.Operation = strings.Repeat("2", 32)
			if _, err := f.m.Execute(context.Background(), r, []byte("replacement")); err == nil {
				t.Fatal("stage accepted while fenced")
			}
			if !bytes.Equal(before, boundaryStore(t, f.m, "notes")) {
				t.Fatal("fenced stage changed existing store")
			}
			if kind == "global-maintenance" {
				if _, err := os.Stat(f.m.path("/var/lib/soar/secrets/other.json")); !os.IsNotExist(err) {
					t.Fatalf("fenced stage created store: %v", err)
				}
			}
			if len(f.events) != 0 {
				t.Fatalf("fenced stage reached external boundaries: %v", f.events)
			}
		})
	}
}

func TestSecretBoundaryResumeMissingPin(t *testing.T) {
	for _, kind := range []string{"store", "version"} {
		t.Run(kind, func(t *testing.T) {
			f, r := flowSetup(t)
			stageSecret(t, f.m, r, "1", secretCanary)
			secretMapping(&r)
			f.buildFail = true
			before := f.execute(r, "archive", true)
			stageSecret(t, f.m, r, "2", "newest-must-not-be-used")
			path := f.m.path("/var/lib/soar/secrets/" + r.App + ".json")
			if kind == "store" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				var disk map[string]json.RawMessage
				if err := json.Unmarshal(boundaryStore(t, f.m, r.App), &disk); err != nil {
					t.Fatal(err)
				}
				var versions map[string]json.RawMessage
				if err := json.Unmarshal(disk["versions"], &versions); err != nil {
					t.Fatal(err)
				}
				delete(versions, strings.Repeat("1", 32))
				disk["versions"], _ = json.Marshal(versions)
				b, err := json.Marshal(disk)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			f.buildFail = false
			f.events = nil
			if _, err := f.m.Execute(context.Background(), flowResume(r), nil); err == nil {
				t.Fatal("resume accepted missing pinned secret")
			}
			if len(f.events) != 0 {
				t.Fatalf("missing pin reached Docker/build: %v", f.events)
			}
			after, err := f.m.load(r.App)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.SecretRefs, after.SecretRefs) {
				t.Fatal("resume replaced pinned refs")
			}
		})
	}
}

func TestSecretBoundaryMappingRetainsCurrent(t *testing.T) {
	for _, kind := range []string{"env-name", "secret-name"} {
		t.Run(kind, func(t *testing.T) {
			f, r := flowSetup(t)
			stageSecret(t, f.m, r, "1", secretCanary)
			secretMapping(&r)
			current := f.execute(r, "first", false).Current
			s := r.Config.Services["web"]
			if kind == "env-name" {
				s.Secrets = map[string]string{"RENAMED": "token"}
			} else {
				q := r
				q.Action = "secrets-stage"
				q.Config = nil
				q.SecretName = "replacement"
				q.Operation = strings.Repeat("2", 32)
				if _, err := f.m.Execute(context.Background(), q, []byte("replacement-value")); err != nil {
					t.Fatal(err)
				}
				s.Secrets = map[string]string{"TOKEN": "replacement"}
			}
			r.Config.Services["web"] = s
			r.Operation = strings.Repeat("e", 32)
			f.unhealthy = true
			pending := f.execute(r, "second", true)
			if !reflect.DeepEqual(pending.Current, current) {
				t.Fatal("mapping change rewrote retained current release before promotion")
			}
			f.unhealthy = false
			ready := f.execute(flowResume(r), "", false)
			if !reflect.DeepEqual(ready.Previous.SecretRefs, current.SecretRefs) {
				t.Fatal("promotion lost previous secret refs")
			}
			if !matchingSecretRefs(ready.Current.SecretRefs, s.Secrets) {
				t.Fatal("promotion did not apply new mapping")
			}
		})
	}
}

func TestSecretBoundaryMaintenanceRetainsStore(t *testing.T) {
	f, r := flowSetup(t)
	stageSecret(t, f.m, r, "1", secretCanary)
	secretMapping(&r)
	f.execute(r, "archive", false)
	stageSecret(t, f.m, r, "2", "pending-private-value")
	before := boundaryStore(t, f.m, r.App)
	// Match the maintenance fake pattern: resource enumeration is an external
	// boundary; planning, route proof, journal and filesystem deletion stay real.
	original := f.m.Run
	f.m.Run = func(c context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "docker" && len(a) > 1 && a[1] == "ls" {
			return process.Result{}, nil
		}
		return original(c, n, a, b)
	}
	for i, action := range []string{"cleanup", "destroy"} {
		q := r
		q.Action = action
		q.Config = nil
		q.Apply = true
		q.Operation = strings.Repeat(string(rune('e'+i)), 32)
		if _, err := f.m.Execute(context.Background(), q, nil); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if !bytes.Equal(before, boundaryStore(t, f.m, r.App)) {
			t.Fatalf("%s changed external store bytes or metadata", action)
		}
		info, err := os.Stat(f.m.path("/var/lib/soar/secrets/" + r.App + ".json"))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
			t.Fatalf("%s exposed store permissions: %v", action, info.Mode())
		}
		q.Action = "secrets-list"
		q.Apply = false
		q.Operation = ""
		response, err := f.m.Execute(context.Background(), q, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.SecretVersions) != 2 {
			t.Fatalf("%s lost retained version metadata: %+v", action, response.SecretVersions)
		}
	}
}
