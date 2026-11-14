package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBatchAtomicRetry(t *testing.T) {
	s := store(t)
	values := map[string][]byte{"b": []byte("second"), "a": []byte("first")}
	entries, err := s.StageBatch("app", hostID, id(1), values)
	if err != nil || len(entries) != 2 || entries[0].Name != "a" || entries[0].Version != VersionFor(id(1), "a") {
		t.Fatalf("batch: %v %v", entries, err)
	}
	if _, err = s.Stage("app", hostID, "a", id(2), []byte("rotated")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, "app.json")
	before, _ := os.ReadFile(path)
	retry, err := s.StageBatch("app", hostID, id(1), values)
	if err != nil || retry[0].Latest || !retry[1].Latest {
		t.Fatalf("retry: %v %v", retry, err)
	}
	for _, bad := range []map[string][]byte{
		{"a": []byte("changed"), "b": []byte("second")},
		{"a": []byte("first"), "c": []byte("new")},
		{"a": []byte("first"), "b": []byte("\n")},
	} {
		if _, err = s.StageBatch("app", hostID, id(1), bad); err == nil {
			t.Fatal("accepted invalid batch")
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("disk changed")
		}
	}
	_, err = s.StageBatch("app", hostID, id(1), map[string][]byte{"a": []byte("first"), "c": []byte("new")})
	if !errors.Is(err, ErrOperationConflict) {
		t.Fatal(err)
	}
	single, err := s.StageBatch("app", hostID, id(3), map[string][]byte{"a": []byte("single")})
	if err != nil || single[0].Version != id(3) {
		t.Fatal(single, err)
	}
}

func TestBatchCodec(t *testing.T) {
	values := map[string][]byte{"a": []byte("<\\\"\t"), "b": []byte("世界")}
	data, err := EncodeBatch(values)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeBatch(data, []string{"b", "a"})
	if err != nil || !reflect.DeepEqual(got, values) {
		t.Fatal(got, err)
	}
	for _, data := range []string{`{"a":"x","a":"y","b":"z"}`, `{"a":"x"}`, `{"a":"x","b":"y","c":"z"}`, `{"a":null,"b":"y"}`, `{"a":"\n","b":"y"}`, `[]`, `null`} {
		if _, err := DecodeBatch([]byte(data), []string{"a", "b"}); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	if _, err := DecodeBatch(data, []string{"a", "a"}); err == nil {
		t.Fatal("duplicate names")
	}
	if _, err := EncodeBatch(nil); err == nil {
		t.Fatal("empty batch")
	}
}

func TestOperationMembershipConflict(t *testing.T) {
	for _, tc := range []struct {
		name         string
		first, retry map[string][]byte
	}{
		{"disjoint", map[string][]byte{"a": []byte("a"), "b": []byte("b")}, map[string][]byte{"c": []byte("c"), "d": []byte("d")}},
		{"batch-single", map[string][]byte{"a": []byte("a"), "b": []byte("b")}, map[string][]byte{"c": []byte("c")}},
		{"single-batch", map[string][]byte{"a": []byte("a")}, map[string][]byte{"c": []byte("c"), "d": []byte("d")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store(t)
			if _, err := s.StageBatch("app", hostID, id(1), tc.first); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.Dir, "app.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.StageBatch("app", hostID, id(1), tc.retry); !errors.Is(err, ErrOperationConflict) {
				t.Fatalf("got %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("disk changed")
			}
		})
	}
}

func TestBatchDiskValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*diskState)
	}{
		{"unsupported", func(d *diskState) { d.Version = 3 }},
		{"v1-batches", func(d *diskState) { d.Version = 1 }},
		{"v2-empty", func(d *diskState) { d.Batches = nil }},
		{"invalid-op", func(d *diskState) { d.Batches = map[string][]string{"bad": {"a", "b"}} }},
		{"unsorted", func(d *diskState) { d.Batches[id(1)] = []string{"b", "a"} }},
		{"duplicate", func(d *diskState) { d.Batches[id(1)] = []string{"a", "a"} }},
		{"singleton", func(d *diskState) { d.Batches[id(1)] = []string{"a"} }},
		{"invalid-name", func(d *diskState) { d.Batches[id(1)] = []string{"a", "bad!"} }},
		{"missing", func(d *diskState) { d.Batches[id(1)] = []string{"a", "c"} }},
		{"wrong-name", func(d *diskState) { d.Versions[VersionFor(id(1), "a")] = diskValue{"b", "x"} }},
		{"op-collision", func(d *diskState) { d.Versions[id(1)] = diskValue{"a", "x"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store(t)
			if _, err := s.StageBatch("app", hostID, id(1), map[string][]byte{"a": []byte("a"), "b": []byte("b")}); err != nil {
				t.Fatal(err)
			}
			state, err := s.Load("app", hostID)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&state.data)
			data, err := json.Marshal(state.data)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.Dir, "app.json")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Load("app", hostID); !errors.Is(err, ErrInvalidStore) {
				t.Fatalf("got %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, after) {
				t.Fatal("disk changed")
			}
		})
	}
}

func TestStoreVersionProgression(t *testing.T) {
	s := store(t)
	if _, err := s.Stage("app", hostID, "a", id(1), []byte("old")); err != nil {
		t.Fatal(err)
	}
	check := func(version int) {
		t.Helper()
		state, err := s.Load("app", hostID)
		if err != nil {
			t.Fatal(err)
		}
		if state.data.Version != version {
			t.Fatalf("version %d", state.data.Version)
		}
		v, err := state.Values(map[string]Ref{"A": {Name: "a", Version: id(1)}})
		if err != nil || v["A"] != "old" {
			t.Fatal(v, err)
		}
	}
	check(1)
	if _, err := s.Stage("app", hostID, "a", id(2), []byte("new")); err != nil {
		t.Fatal(err)
	}
	check(1)
	if _, err := s.StageBatch("app", hostID, id(3), map[string][]byte{"b": []byte("b"), "c": []byte("c")}); err != nil {
		t.Fatal(err)
	}
	check(2)
	if _, err := s.Stage("app", hostID, "a", id(4), []byte("latest")); err != nil {
		t.Fatal(err)
	}
	check(2)
}
