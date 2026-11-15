package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func testServer(name string) Server {
	return Server{Name: name, Target: "root@" + name, Domain: name + ".example.com"}
}

func readState(t *testing.T, store *Store) State {
	t.Helper()
	state, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestStorePersistenceAndDefault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config", "vamos")
	store := New(dir)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("New should not create the directory: %v", err)
	}
	want := State{Version: 1, Servers: map[string]Server{}}
	if got := readState(t, store); !reflect.DeepEqual(got, want) {
		t.Fatalf("empty registry = %#v, want %#v", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, registryFile)); !os.IsNotExist(err) {
		t.Fatalf("Read should not create a state file: %v", err)
	}
	for _, name := range []string{"zebra", "alpha", "middle"} {
		server := testServer(name)
		if err := store.Put(server); err != nil {
			t.Fatal(err)
		}
		want.Servers[name] = server
	}
	want.Default = "zebra"
	if got := readState(t, New(dir)); !reflect.DeepEqual(got, want) {
		t.Fatalf("persisted registry = %#v, want %#v", got, want)
	}
	snapshot := readState(t, store)
	delete(snapshot.Servers, "zebra")
	if got := readState(t, store); !reflect.DeepEqual(got, want) {
		t.Fatal("mutating a snapshot changed the store")
	}
	data := readBytes(t, filepath.Join(dir, registryFile))
	if bytes.Contains(data, []byte(`"Name"`)) || !bytes.Contains(data, []byte(`"name"`)) {
		t.Fatalf("expected lowercase JSON fields: %s", data)
	}
}

func TestDefaultStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("HOME", dir)
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	store, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if store.dir != filepath.Join(configDir, "vamos") {
		t.Fatalf("default directory = %q", store.dir)
	}
	if _, err := os.Stat(store.dir); !os.IsNotExist(err) {
		t.Fatalf("Default should not create the directory: %v", err)
	}
}

func TestPutIdempotenceAndConflicts(t *testing.T) {
	store := New(t.TempDir())
	server := testServer("primary")
	if err := store.Put(server); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.dir, registryFile)
	before := readBytes(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(server); err != nil {
		t.Fatalf("identical Put: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, after) || !bytes.Equal(before, readBytes(t, path)) {
		t.Fatal("idempotent Put rewrote the state file")
	}
	for _, conflicting := range []Server{
		{Name: server.Name, Target: "root@different", Domain: server.Domain},
		{Name: server.Name, Target: server.Target, Domain: "different.example.com"},
		{Name: "alias", Target: server.Target, Domain: server.Domain},
		{Name: "alias", Target: server.Target, Domain: "different.example.com"},
	} {
		if err := store.Put(conflicting); err == nil {
			t.Errorf("accepted conflicting server %#v", conflicting)
		}
		if !bytes.Equal(before, readBytes(t, path)) {
			t.Fatal("conflicting Put changed the registry")
		}
	}
	// Targets are compared exactly; domains need not be unique. Neither is
	// interpreted as shell syntax or validated as a network endpoint here.
	other := Server{Name: "other", Target: "Root@primary;literal", Domain: server.Domain}
	if err := store.Put(other); err != nil {
		t.Fatalf("distinct literal target: %v", err)
	}
}

func TestRemove(t *testing.T) {
	store := New(t.TempDir())
	if err := store.Remove("missing"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.dir, registryFile)); !os.IsNotExist(err) {
		t.Fatalf("removing absent entry created state: %v", err)
	}
	for _, name := range []string{"zebra", "middle", "beta", "alpha"} {
		if err := store.Put(testServer(name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []struct {
		name, wantDefault string
		count             int
	}{
		{"middle", "zebra", 3},
		{"missing", "zebra", 3},
		{"zebra", "alpha", 2},
		{"alpha", "beta", 1},
		{"beta", "", 0},
		{"beta", "", 0},
	} {
		if err := store.Remove(step.name); err != nil {
			t.Fatal(err)
		}
		state := readState(t, New(store.dir))
		if state.Default != step.wantDefault || len(state.Servers) != step.count || state.Version != 1 {
			t.Fatalf("after removing %q: %#v", step.name, state)
		}
	}
	if err := store.Put(testServer("new")); err != nil {
		t.Fatal(err)
	}
	if state := readState(t, store); state.Default != "new" {
		t.Fatalf("new first server is not default: %#v", state)
	}
}

func TestPutValidation(t *testing.T) {
	for _, name := range []string{"", "Upper", "bad_name", "-first", "last-", "a.b", "a/b", "a b", "é", strings.Repeat("a", 64)} {
		t.Run("name-"+name, func(t *testing.T) {
			store := New(t.TempDir())
			if err := store.Put(testServer(name)); err == nil {
				t.Fatalf("accepted invalid name %q", name)
			}
			if got := readState(t, store); len(got.Servers) != 0 {
				t.Fatal("invalid Put changed state")
			}
		})
	}
	for _, server := range []Server{
		{Name: "valid", Target: "", Domain: "example.com"},
		{Name: "valid", Target: "root@host", Domain: ""},
		{Name: "valid", Target: " \t", Domain: "example.com"},
		{Name: "valid", Target: "root@host", Domain: "\n"},
		{Name: "valid", Target: "root@\xff", Domain: "example.com"},
		{Name: "valid", Target: "root@host", Domain: "\xff.example.com"},
	} {
		if err := New(t.TempDir()).Put(server); err == nil {
			t.Errorf("accepted invalid server %#v", server)
		}
	}
	for _, name := range []string{"a", "0", "a-0", strings.Repeat("a", 63)} {
		if err := New(t.TempDir()).Put(testServer(name)); err != nil {
			t.Errorf("valid name %q: %v", name, err)
		}
	}
}

func TestRejectInvalidExistingStateWithoutChangingIt(t *testing.T) {
	valid := `{"version":1,"default":"a","servers":{"a":{"name":"a","target":"root@a","domain":"a.example.com"}}}`
	tests := map[string]string{
		"invalid UTF-8":           strings.Replace(valid, "root@a", "root@\xff", 1),
		"empty":                   "",
		"null":                    "null",
		"array":                   "[]",
		"truncated":               `{"version":1`,
		"trailing document":       valid + `{}`,
		"trailing garbage":        valid + `oops`,
		"missing version":         `{"default":"","servers":{}}`,
		"missing default":         `{"version":1,"servers":{}}`,
		"missing servers":         `{"version":1,"default":""}`,
		"null version":            `{"version":null,"default":"","servers":{}}`,
		"null default":            `{"version":1,"default":null,"servers":{}}`,
		"null servers":            `{"version":1,"default":"","servers":null}`,
		"wrong servers type":      `{"version":1,"default":"","servers":[]}`,
		"future version":          strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"zero version":            strings.Replace(valid, `"version":1`, `"version":0`, 1),
		"string version":          strings.Replace(valid, `"version":1`, `"version":"1"`, 1),
		"fractional version":      strings.Replace(valid, `"version":1`, `"version":1.5`, 1),
		"unknown field":           strings.Replace(valid, `"version":1`, `"extra":true,"version":1`, 1),
		"wrong case":              strings.Replace(valid, `"version"`, `"Version"`, 1),
		"duplicate field":         strings.Replace(valid, `"version":1`, `"version":2,"version":1`, 1),
		"unknown server field":    strings.Replace(valid, `"name":"a"`, `"name":"a","extra":true`, 1),
		"duplicate name":          strings.Replace(valid, `"name":"a"`, `"name":"b","name":"a"`, 1),
		"duplicate server":        `{"version":1,"default":"a","servers":{"a":{},"a":{"name":"a","target":"a","domain":"a"}}}`,
		"missing target":          strings.Replace(valid, `"target":"root@a",`, ``, 1),
		"null target":             strings.Replace(valid, `"target":"root@a"`, `"target":null`, 1),
		"empty target":            strings.Replace(valid, `root@a`, ``, 1),
		"invalid domain type":     strings.Replace(valid, `"domain":"a.example.com"`, `"domain":1`, 1),
		"empty domain":            strings.Replace(valid, `a.example.com`, ``, 1),
		"wrong name":              strings.Replace(valid, `"name":"a"`, `"name":"b"`, 1),
		"invalid name":            strings.ReplaceAll(valid, `"a"`, `"BAD"`),
		"missing default name":    strings.Replace(valid, `"default":"a"`, `"default":"b"`, 1),
		"empty default":           strings.Replace(valid, `"default":"a"`, `"default":""`, 1),
		"default without servers": `{"version":1,"default":"a","servers":{}}`,
		"duplicate targets": `{"version":1,"default":"a","servers":{
			"a":{"name":"a","target":"same","domain":"a"},
			"b":{"name":"b","target":"same","domain":"b"}}}`,
		"oversized": strings.Repeat(" ", maxSize+1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			store := New(t.TempDir())
			path := filepath.Join(store.dir, registryFile)
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Read(); err == nil {
				t.Fatal("Read accepted invalid state")
			}
			if err := store.Put(testServer("new")); err == nil {
				t.Fatal("Put accepted invalid existing state")
			}
			for _, name := range []string{"a", "missing"} {
				if err := store.Remove(name); err == nil {
					t.Fatal("Remove accepted invalid existing state")
				}
			}
			if got := readBytes(t, path); !bytes.Equal(got, []byte(input)) {
				t.Fatal("invalid registry was modified")
			}
		})
	}
}

func TestOversizedPutPreservesState(t *testing.T) {
	store := New(t.TempDir())
	if err := store.Put(testServer("first")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.dir, registryFile)
	before := readBytes(t, path)
	large := testServer("large")
	large.Domain = strings.Repeat("x", maxSize)
	if err := store.Put(large); err == nil {
		t.Fatal("accepted oversized state")
	}
	if !bytes.Equal(before, readBytes(t, path)) {
		t.Fatal("oversized write changed the registry")
	}
}

func TestPermissionsAndStableLock(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "vamos"))
	if err := os.Mkdir(store.dir, 0755); err != nil {
		t.Fatal(err)
	}
	readState(t, store)
	lockPath := filepath.Join(store.dir, lockFile)
	lockBefore, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if err := store.Put(testServer(name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Remove("first"); err != nil {
		t.Fatal(err)
	}
	lockAfter, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("lock file was replaced")
	}
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("unexpected files (temporary file leaked?): %v", entries)
	}
	if runtime.GOOS == "windows" {
		return // Windows permissions use ACLs, not Unix mode bits.
	}
	for path, want := range map[string]os.FileMode{store.dir: 0700, lockPath: 0600, filepath.Join(store.dir, registryFile): 0600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s permissions = %o, want %o", path, got, want)
		}
	}
}

func TestSymlinkStateRejected(t *testing.T) {
	for _, dangling := range []bool{false, true} {
		t.Run(fmt.Sprint("dangling-", dangling), func(t *testing.T) {
			store := New(t.TempDir())
			target := filepath.Join(t.TempDir(), "other.json")
			original := []byte(`{"version":1,"default":"","servers":{}}`)
			if !dangling {
				if err := os.WriteFile(target, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(store.dir, registryFile)
			if err := os.Symlink(target, path); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlink privileges unavailable: %v", err)
				}
				t.Fatal(err)
			}
			if _, err := store.Read(); err == nil {
				t.Fatal("Read followed symlink")
			}
			if err := store.Put(testServer("new")); err == nil {
				t.Fatal("Put accepted symlink")
			}
			if err := store.Remove("missing"); err == nil {
				t.Fatal("Remove accepted symlink")
			}
			if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("symlink was changed: %v", err)
			}
			if dangling {
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatalf("dangling target was created: %v", err)
				}
			} else if !bytes.Equal(original, readBytes(t, target)) {
				t.Fatal("symlink target was changed")
			}
		})
	}
}

func TestNonregularStateRejected(t *testing.T) {
	store := New(t.TempDir())
	if err := os.Mkdir(filepath.Join(store.dir, registryFile), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(); err == nil {
		t.Fatal("Read accepted a directory as state")
	}
	if err := store.Put(testServer("new")); err == nil {
		t.Fatal("Put accepted a directory as state")
	}
}

func TestAllOperationsTakeLock(t *testing.T) {
	for _, operation := range []string{"read", "put", "remove"} {
		t.Run(operation, func(t *testing.T) {
			store := New(t.TempDir())
			readState(t, store)
			lock := flock.New(filepath.Join(store.dir, lockFile))
			if err := lock.Lock(); err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(started)
				switch operation {
				case "read":
					_, err := store.Read()
					done <- err
				case "put":
					done <- store.Put(testServer("new"))
				case "remove":
					done <- store.Remove("missing")
				}
			}()
			<-started
			select {
			case err := <-done:
				t.Fatalf("operation did not wait for lock: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("operation did not finish after lock release")
			}
		})
	}
}

func TestConcurrentStores(t *testing.T) {
	dir := t.TempDir()
	stores := []*Store{New(dir), New(dir)}
	const count = 40
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store := stores[i%len(stores)]
			server := testServer(fmt.Sprintf("server-%02d", i))
			if err := store.Put(server); err != nil {
				t.Error(err)
				return
			}
			if _, err := stores[(i+1)%len(stores)].Read(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	state := readState(t, New(dir))
	if len(state.Servers) != count {
		t.Fatalf("lost concurrent additions: got %d", len(state.Servers))
	}
	if _, ok := state.Servers[state.Default]; !ok {
		t.Fatalf("invalid default %q", state.Default)
	}
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := stores[i%len(stores)].Remove(fmt.Sprintf("server-%02d", i)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	state = readState(t, New(dir))
	if len(state.Servers) != 0 || state.Default != "" {
		t.Fatalf("lost concurrent removals: %#v", state)
	}
}

func TestConcurrentDuplicateTarget(t *testing.T) {
	dir := t.TempDir()
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, name := range []string{"first", "second"} {
		go func() {
			<-start
			results <- New(dir).Put(Server{Name: name, Target: "same", Domain: "example.com"})
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("got %d successful duplicate registrations, want 1", successes)
	}
	state := readState(t, New(dir))
	if len(state.Servers) != 1 {
		t.Fatalf("unexpected state: %#v", state)
	}
	data := readBytes(t, filepath.Join(dir, registryFile))
	if !json.Valid(data) {
		t.Fatal("concurrent writes left malformed JSON")
	}
}
