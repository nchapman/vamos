package backupbackend

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallIndependentOfRuntimeFilesystem(t *testing.T) {
	payload := []byte("synthetic installer payload\n")
	compressed, err := base64.StdEncoding.DecodeString("QlpoOTFBWSZTWdKIQj8AAAtRgAAQQAAuZdwgIAAih5IG0IUwAE0JTzD9Sazg1rAMTNZHB8XckU4UJDSiEI/A")
	if err != nil {
		t.Fatal(err)
	}
	versionDir := t.TempDir()
	scratch := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(scratch, []byte("runtime untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entries, err := os.ReadDir(versionDir)
		if err != nil {
			t.Error(err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				st, err := entry.Info()
				if err != nil || st.Mode().Perm() != 0700 {
					t.Errorf("staging must be private: %v, %v", st, err)
				}
			}
		}
		w.Write(compressed)
	}))
	defer server.Close()
	a := Artifact{URL: server.URL, CompressedSHA256: fmt.Sprintf("%x", sha256.Sum256(compressed)), SHA256: fmt.Sprintf("%x", sha256.Sum256(payload))}
	dest, err := installArtifact(context.Background(), versionDir, scratch, a)
	if err != nil {
		t.Fatalf("publication must not depend on runtime scratch being a directory: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("payload = %q, error = %v", got, err)
	}
	st, err := os.Stat(dest)
	if err != nil || st.Mode().Perm() != 0755 {
		t.Fatalf("executable mode: %v, %v", st, err)
	}
	// A second publication must fail without replacing even a drifted destination.
	if err := os.WriteFile(dest, []byte("existing"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := installArtifact(context.Background(), versionDir, scratch, a); err == nil {
		t.Fatal("overwrote existing destination")
	}
	got, err = os.ReadFile(dest)
	if err != nil || string(got) != "existing" {
		t.Fatalf("destination changed: %q, %v", got, err)
	}
	entries, err := os.ReadDir(versionDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "restic" {
		t.Fatalf("staging not cleaned: %v, %v", entries, err)
	}
	got, err = os.ReadFile(scratch)
	if err != nil || string(got) != "runtime untouched" {
		t.Fatalf("runtime changed: %q, %v", got, err)
	}
}
