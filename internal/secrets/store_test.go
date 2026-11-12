package secrets

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const hostID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func store(t *testing.T) Store {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Store{filepath.Join(root, "secrets")}
}
func id(n int) string { return fmt.Sprintf("%032x", n) }
func TestInput(t *testing.T) {
	for _, v := range []string{"", "\n", "x\n\n", "x\r", "x\x00", "\xff", strings.Repeat("x", 8193)} {
		if _, err := ReadInput(strings.NewReader(v)); err == nil {
			t.Fatal("accepted invalid input")
		}
	}
	for _, v := range []string{" x\t $'\\\" ", strings.Repeat("x", 8192)} {
		for _, suffix := range []string{"", "\n", "\r\n"} {
			got, err := ReadInput(strings.NewReader(v + suffix))
			if err != nil || string(got) != v {
				t.Fatal("framing changed literal")
			}
		}
	}
}
func TestRotation(t *testing.T) {
	s := store(t)
	literal := " $'\\\"\t synthetic-canary "
	if _, err := s.Stage("app", hostID, "key", id(1), []byte(literal)); err != nil {
		t.Fatal(err)
	}
	state, err := s.Load("app", hostID)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := state.Snapshot(map[string]string{"KEY": "key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Stage("app", hostID, "key", id(2), []byte("new")); err != nil {
		t.Fatal(err)
	}
	entry, err := s.Stage("app", hostID, "key", id(1), []byte(literal))
	if err != nil || entry.Latest {
		t.Fatal("retry moved latest")
	}
	state, err = s.Load("app", hostID)
	if err != nil {
		t.Fatal(err)
	}
	values, err := state.Values(refs)
	if err != nil || values["KEY"] != literal {
		t.Fatal("immutable value lost")
	}
	if len(state.List()) != 2 || !state.List()[1].Latest {
		t.Fatal("incorrect metadata")
	}
	data, _ := json.Marshal(state)
	if strings.Contains(string(data), "canary") {
		t.Fatal("plaintext marshaled")
	}
	if _, err = s.Stage("app", hostID, "key", id(1), []byte("different")); err == nil {
		t.Fatal("accepted collision")
	}
	if _, err = state.Values(map[string]Ref{"KEY": {"other", id(1)}}); err == nil {
		t.Fatal("accepted wrong name")
	}
	var absent *State
	if _, err = absent.Snapshot(nil); err != nil {
		t.Fatal(err)
	}
	if _, err = absent.Snapshot(map[string]string{"KEY": "key"}); err == nil {
		t.Fatal("accepted missing secret")
	}
}
func TestInvalidStore(t *testing.T) {
	s := store(t)
	if _, err := s.Stage("app", hostID, "key", id(1), []byte("synthetic-canary")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, "app.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"synthetic-canary", `{"synthetic-canary":true}`, strings.Replace(string(original), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(original), hostID, id(4), 1), string(original) + " {}", strings.Repeat("x", MaxStoreBytes+1)} {
		if err = os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		_, err = s.Load("app", hostID)
		if err == nil || strings.Contains(err.Error(), "synthetic-canary") {
			t.Fatal("invalid content accepted or disclosed")
		}
	}
}
func TestUnsafeFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	for _, kind := range []string{"file-mode", "dir-mode", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			s := store(t)
			if _, err := s.Stage("app", hostID, "key", id(1), []byte("v")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.Dir, "app.json")
			switch kind {
			case "file-mode":
				os.Chmod(path, 0644)
			case "dir-mode":
				os.Chmod(s.Dir, 0755)
			case "symlink":
				os.Remove(path)
				os.Symlink("missing", path)
			case "directory":
				os.Remove(path)
				os.Mkdir(path, 0600)
			}
			if _, err := s.Load("app", hostID); err == nil {
				t.Fatal("unsafe store accepted")
			}
			if _, err := s.Stage("app", hostID, "key", id(2), []byte("v")); err == nil {
				t.Fatal("unsafe store overwritten")
			}
		})
	}
}
func TestBudgets(t *testing.T) {
	for _, kind := range []string{"names", "versions", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			s := store(t)
			limit := MaxVersions
			value := "v"
			if kind == "names" {
				limit = MaxNames
			}
			if kind == "bytes" {
				limit = MaxVersions
				value = strings.Repeat("\t", MaxValueBytes)
			}
			for n := 0; n <= limit; n++ {
				name := "key"
				if kind == "names" {
					name = fmt.Sprintf("key-%d", n)
				}
				path := filepath.Join(s.Dir, "app.json")
				before, _ := os.ReadFile(path)
				_, err := s.Stage("app", hostID, name, id(n), []byte(value))
				if err != nil {
					if kind != "bytes" && n != limit {
						t.Fatal(err)
					}
					if !strings.Contains(err.Error(), "budget") {
						t.Fatal(err)
					}
					after, _ := os.ReadFile(path)
					if string(before) != string(after) {
						t.Fatal("budget failure changed state")
					}
					return
				}
			}
			t.Fatal("budget not enforced")
		})
	}
}
