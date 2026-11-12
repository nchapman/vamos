package secrets

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStoreRejectsWritableParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	s := store(t)
	if err := os.Chmod(filepath.Dir(s.Dir), 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stage("app", hostID, "key", id(1), []byte("synthetic")); err == nil {
		t.Fatal("accepted replaceable secret directory")
	}
}
