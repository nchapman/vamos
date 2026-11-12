package appstate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTombstoneDecodeIsStrictAndBounded(t *testing.T) {
	id := strings.Repeat("a", 32)
	valid := Tombstone{Version: 1, App: "notes", Operation: strings.Repeat("b", 32), Phase: "destroyed", HostID: id}
	b, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(strings.NewReader(string(b)), "notes", id); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{string(b) + " {}", string(b[:len(b)-1]) + `,"Current":null}`, strings.Replace(string(b), "destroyed", "destroying", 1), strings.Repeat(" ", 4097)} {
		if _, err := Decode(strings.NewReader(raw), "notes", id); err == nil {
			t.Fatalf("accepted invalid receipt %q", raw)
		}
	}
	if _, err := Decode(strings.NewReader(string(b)), "other", id); err == nil {
		t.Fatal("accepted foreign app")
	}
	if _, err := Decode(strings.NewReader(string(b)), "notes", strings.Repeat("f", 32)); err == nil {
		t.Fatal("accepted foreign host")
	}
}
