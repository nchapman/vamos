package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/backup"
	"github.com/nchapman/vamos/internal/backupbackend"
)

func TestRepositoryPrivateFileBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("root Linux executor file permissions")
	}
	credentials := backupbackend.Credentials{AccessKeyID: "synthetic", SecretAccessKey: "private-canary", Password: strings.Repeat("a", 64)}
	data, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"valid", "mode", "oversized", "symlink", "invalid", "directory"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "repository.credentials")
			contents := data
			if kind == "oversized" {
				contents = []byte(strings.Repeat("x", backup.MaxPrivateBytes+1))
			}
			if kind == "invalid" {
				contents = []byte(`{"private-canary":"unknown"}`)
			}
			if err := os.WriteFile(path, contents, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "mode":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := path + "-target"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readRepositoryFile(path)
			if kind == "valid" {
				if err != nil || string(got) != string(data) {
					t.Fatal("valid private input rejected", err)
				}
				return
			}
			if err == nil || strings.Contains(err.Error(), "private-canary") {
				t.Fatal("unsafe private input handling", err)
			}
		})
	}
}
