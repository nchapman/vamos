package executor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadSecretFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux executor requires Unix file permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.value")
	if err := os.WriteFile(path, []byte("  preserve  "), 0600); err != nil {
		t.Fatal(err)
	}
	value, err := readSecretFile(path)
	if err != nil || string(value) != "  preserve  " {
		t.Fatalf("read failed: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(path); err == nil {
		t.Fatal("public file accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 8193)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(path); err == nil {
		t.Fatal("oversize accepted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(link); err == nil {
		t.Fatal("symlink accepted")
	}
}
