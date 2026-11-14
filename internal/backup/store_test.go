package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrivateStoreRejectsUnsafeFilesAndDirectories(t *testing.T) {
	for _, kind := range []string{"file-mode", "directory-mode", "parent-writable", "symlink", "corruption", "unknown-file", "foreign-host"} {
		t.Run(kind, func(t *testing.T) {
			m, _, r, data, host := setupRepository(t)
			if _, err := m.Execute(context.Background(), host, r, data); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(m.storage().dir, r.Name+".json")
			switch kind {
			case "file-mode":
				if runtime.GOOS == "windows" {
					t.Skip("POSIX modes")
				}
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "directory-mode":
				if runtime.GOOS == "windows" {
					t.Skip("POSIX modes")
				}
				if err := os.Chmod(m.storage().dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "parent-writable":
				if runtime.GOOS == "windows" {
					t.Skip("POSIX modes")
				}
				if err := os.Chmod(filepath.Join(m.Root, "var/lib/soar"), 0777); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if runtime.GOOS == "windows" {
					t.Skip("symlink privilege")
				}
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-original", path); err != nil {
					t.Fatal(err)
				}
			case "corruption":
				if err := os.WriteFile(path, []byte(`{"credentials":"secret-must-not-leak"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-file":
				if err := os.WriteFile(filepath.Join(m.storage().dir, "surprise"), []byte("secret-must-not-leak"), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-host":
				host = strings.Repeat("e", 32)
			}
			_, err := m.Execute(context.Background(), host, Request{Action: "list"}, nil)
			if !errors.Is(err, errStore) || strings.Contains(err.Error(), "secret-must-not-leak") {
				t.Fatal("unsafe store accepted or leaked data", err)
			}
		})
	}
}

func TestRetainedPrivateWriteIsNotAuthoritative(t *testing.T) {
	m, _, r, data, host := setupRepository(t)
	if _, err := m.Execute(context.Background(), host, r, data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.storage().dir, ".tmp-interrupted"), []byte("not committed"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := m.Execute(context.Background(), host, Request{Action: "list"}, nil)
	if err != nil || len(result.Repositories) != 1 {
		t.Fatal(err)
	}
}

func TestStateCannotEraseInitializationIntentOrChangeCredentials(t *testing.T) {
	m, _, r, data, host := setupRepository(t)
	if _, err := m.Execute(context.Background(), host, r, data); err != nil {
		t.Fatal(err)
	}
	saved, err := m.storage().load(r.Name, host)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*record){func(r *record) { r.InitAttempted = false; r.Metadata.State = "prepared" }, func(r *record) { r.Credentials.Password = strings.Repeat("e", 64) }, func(r *record) { r.Metadata.RepositoryID = strings.Repeat("f", 64) }} {
		next := *saved
		change(&next)
		if err := m.storage().save(&next); !errors.Is(err, errStore) {
			t.Fatal("mutable captured identity", err)
		}
	}
}

func TestRequestAndCredentialBoundaries(t *testing.T) {
	_, _, request, data, _ := setupRepository(t)
	for _, change := range []func(*Request){
		func(r *Request) { r.Name = "../escape" }, func(r *Request) { r.Confirm = false }, func(r *Request) { r.Action = "destroy" },
		func(r *Request) { r.Config = nil }, func(r *Request) { r.Operation = "latest" }, func(r *Request) { r.ExpectedID = strings.Repeat("a", 64) },
		func(r *Request) { cfg := *r.Config; cfg.Prefix = "shared"; r.Config = &cfg },
	} {
		r := request
		change(&r)
		if ValidateRequest(r) == nil {
			t.Fatal("invalid request accepted")
		}
	}
	for _, bad := range [][]byte{nil, []byte(`{"password":"secret-value","unexpected":true}`), append(append([]byte{}, data...), data...), []byte(strings.Repeat("x", MaxPrivateBytes+1))} {
		if _, err := DecodeCredentials(bad); err == nil || strings.Contains(err.Error(), "secret-value") {
			t.Fatal("invalid credential boundary", err)
		}
	}
	if _, err := DecodeCredentials(data); err != nil {
		t.Fatal(err)
	}
	r := Request{Action: "connect", Name: "offsite", Operation: request.Operation, Config: request.Config, Confirm: true, ExpectedID: strings.Repeat("a", 64)}
	if ValidateRequest(r) != nil {
		t.Fatal("valid connect rejected")
	}
	for _, action := range []string{"status", "verify", "resume", "list"} {
		r := Request{Action: action, Name: "offsite"}
		if action == "resume" {
			r.Operation = request.Operation
		}
		if action == "list" {
			r.Name = ""
		}
		if ValidateRequest(r) != nil {
			t.Fatal("valid observation rejected")
		}
		r.Confirm = true
		if ValidateRequest(r) == nil {
			t.Fatal("observation accepts mutation authorization")
		}
	}
}

func TestStoreContainsCredentialsOnlyInPrivateRecord(t *testing.T) {
	m, _, r, data, host := setupRepository(t)
	if _, err := m.Execute(context.Background(), host, r, data); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.storage().dir, r.Name+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("nonprivate record")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved record
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.HostID != host || saved.Credentials.Password != strings.Repeat("c", 64) {
		t.Fatal("recovery material missing")
	}
}
