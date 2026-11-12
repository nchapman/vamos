package deploy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSavedContextRejectsOversizedArchive(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "context.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((32 << 20) + 4096); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if _, err := readContextArchive(path); err == nil {
		t.Fatal("accepted oversized saved context")
	}
	if err := os.WriteFile(path, []byte("small archive"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := readContextArchive(path); err != nil || string(data) != "small archive" {
		t.Fatalf("data=%q error=%v", data, err)
	}
}
