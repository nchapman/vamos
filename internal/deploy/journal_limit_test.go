package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveRejectsUnreadableJournalWithoutReplacingState(t *testing.T) {
	f, r := flowSetup(t)
	j := f.execute(r, "context", false)
	path := filepath.Join(f.m.dir(r.App), "state.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	j.OldCaddy = strings.Repeat("x", 2<<20)
	j.NewCaddy = j.OldCaddy
	if err := f.m.save(j); err == nil {
		t.Fatal("saved a journal exceeding the reader limit")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("oversize save replaced durable state")
	}
}
