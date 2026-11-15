package volumes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixture() *State {
	return &State{Version: 1, App: "app", HostID: strings.Repeat("a", 32), Records: map[string]Record{"web": {ID: strings.Repeat("b", 32), Service: "web", Name: "data", Mount: "/data", Operation: strings.Repeat("c", 32), State: "planned"}}}
}
func storeFor(t *testing.T) Store {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Store{Root: filepath.Join(root, "volumes")}
}
func TestRoundTrip(t *testing.T) {
	s := storeFor(t)
	state := fixture()
	if got, err := s.Load(state.App, state.HostID); got != nil || err != nil {
		t.Fatalf("missing: %v %v", got, err)
	}
	for _, created := range []bool{false, true} {
		if created {
			r := state.Records["web"]
			r.State = "created"
			r.CreatedAt = "2026-01-01T00:00:00Z"
			state.Records["web"] = r
		}
		if err := s.Save(state); err != nil {
			t.Fatal(err)
		}
		got, err := s.Load(state.App, state.HostID)
		if err != nil || !reflect.DeepEqual(got, state) {
			t.Fatalf("roundtrip: %v %v", got, err)
		}
	}
	if state.Records["web"].DockerName() != "vamos-vol-"+strings.Repeat("b", 32) {
		t.Fatal("docker name")
	}
}
func TestValidation(t *testing.T) {
	tests := map[string]func(*State){
		"version": func(s *State) { s.Version = 2 }, "host": func(s *State) { s.HostID = "foreign" }, "nil": func(s *State) { s.Records = nil },
		"key": func(s *State) { s.Records["other"] = s.Records["web"] },
		"capacity": func(s *State) {
			for i := 0; i < 65; i++ {
				n := fmt.Sprintf("s%d", i)
				s.Records[n] = Record{ID: fmt.Sprintf("%032x", i), Service: n, Name: n, Mount: "/data", Operation: strings.Repeat("a", 32), State: "planned"}
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := fixture()
			mutate(s)
			if Validate(s) == nil {
				t.Fatal("accepted")
			}
		})
	}
	for _, change := range []func(*Record){func(r *Record) { r.Mount = "/" }, func(r *Record) { r.Mount = "/a/../b" }, func(r *Record) { r.ID = strings.Repeat("A", 32) }, func(r *Record) { r.State = "created" }, func(r *Record) { r.CreatedAt = "2026-01-01T00:00:00Z" }, func(r *Record) { r.Mount = "/" + strings.Repeat("x", MaxStoreBytes) }} {
		s := fixture()
		r := s.Records["web"]
		change(&r)
		s.Records["web"] = r
		if Validate(s) == nil {
			t.Fatal("accepted invalid record")
		}
	}
}
func TestPersistenceRefusesUnsafeAndForeign(t *testing.T) {
	for _, kind := range []string{"foreign", "duplicate", "unknown", "oversize", "symlink", "mode", "invalid-save"} {
		t.Run(kind, func(t *testing.T) {
			store := storeFor(t)
			s := fixture()
			if err := store.Save(s); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(store.Root, "app.json")
			data, _ := os.ReadFile(file)
			switch kind {
			case "foreign":
				s.HostID = strings.Repeat("d", 32)
			case "duplicate":
				data = []byte(strings.Replace(string(data), `"version":1`, `"version":1,"version":1`, 1))
				os.WriteFile(file, data, 0600)
			case "unknown":
				data = []byte(strings.Replace(string(data), `"version":1`, `"typo":1,"version":1`, 1))
				os.WriteFile(file, data, 0600)
			case "oversize":
				data = []byte(strings.Repeat(" ", MaxStoreBytes+1))
				os.WriteFile(file, data, 0600)
			case "symlink":
				target := filepath.Join(filepath.Dir(store.Root), "target")
				os.WriteFile(target, data, 0600)
				os.Remove(file)
				if err := os.Symlink(target, file); err != nil {
					t.Fatal(err)
				}
			case "mode":
				os.Chmod(file, 0644)
			case "invalid-save":
				s.Version = 2
			}
			if err := store.Save(s); err == nil {
				t.Fatal("save accepted")
			}
			after, err := os.ReadFile(file)
			if err != nil || string(after) != string(data) {
				t.Fatal("changed existing file")
			}
			entries, _ := os.ReadDir(store.Root)
			if len(entries) != 1 {
				t.Fatal("left temporary files")
			}
		})
	}
}
func TestStrictNestedJSON(t *testing.T) {
	store := storeFor(t)
	s := fixture()
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(s)
	for _, bad := range []string{strings.Replace(string(data), `"mount":"/data"`, `"mount":"/data","mount":"/data"`, 1), strings.Replace(string(data), `"mount":"/data"`, `"mount":"/data","extra":true`, 1), string(data) + ` {}`} {
		if err := os.WriteFile(filepath.Join(store.Root, "app.json"), []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(s.App, s.HostID); err == nil {
			t.Fatal("accepted malformed JSON")
		}
	}
}
