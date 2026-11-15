//go:build !windows

package session

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestUnsafeStagingRefusedAndLeftIntact(t *testing.T) {
	for _, item := range []string{"root", "directory", ".lock", "lock", "executor", "prepared.json"} {
		for _, damage := range []string{"mode", "symlink"} {
			t.Run(item+"/"+damage, func(t *testing.T) {
				f := setup(t)
				f.prepare(t, testToken)
				path := filepath.Join(f.m.Root, testToken, item)
				switch item {
				case "root":
					path = f.m.Root
				case "directory":
					path = filepath.Join(f.m.Root, testToken)
				case ".lock":
					path = filepath.Join(f.m.Root, ".lock")
				}
				if damage == "mode" {
					mode := os.FileMode(0644)
					if item == "root" || item == "directory" {
						mode = 0755
					}
					if err := os.Chmod(path, mode); err != nil {
						t.Fatal(err)
					}
				} else {
					saved := path + ".saved"
					if err := os.Rename(path, saved); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(saved, path); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.m.Attach(context.Background(), testToken, nil, io.Discard, io.Discard); err == nil {
					t.Fatal("unsafe attach accepted")
				}
				if err := f.m.Cleanup(testToken); err == nil {
					t.Fatal("unsafe cleanup accepted")
				}
				f.now = f.now.Add(preparationTTL)
				if _, err := f.m.Prepare(context.Background(), strings.Repeat("b", 32), f.req, f.exe); err == nil {
					t.Fatal("unsafe prune accepted")
				}
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("unsafe path removed: %v", err)
				}
				if _, err := os.Stat(filepath.Join(f.m.Root, testToken, "executor")); err != nil {
					t.Fatalf("staging removed: %v", err)
				}
				if f.runs != 0 {
					t.Fatal("unsafe staging invoked")
				}
			})
		}
	}
}

func TestUnexpectedRootEntryRequiresOperatorCorrection(t *testing.T) {
	f := setup(t)
	f.prepare(t, testToken)
	foreign := filepath.Join(f.m.Root, "not-a-vamos-token")
	if err := os.WriteFile(foreign, []byte("preserve operator data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Prepare(context.Background(), strings.Repeat("b", 32), f.req, f.exe); err == nil {
		t.Fatal("accepted a session root containing non-Vamos state")
	}
	data, err := os.ReadFile(foreign)
	if err != nil || string(data) != "preserve operator data" {
		t.Fatalf("modified foreign root entry: %q %v", data, err)
	}
	if f.runs != 0 {
		t.Fatal("unexpected root state ran a workload")
	}
}

func TestCleanupNeverTraversesUnexpectedChildren(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "file"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			f.prepare(t, testToken)
			outside := filepath.Join(t.TempDir(), "precious")
			if err := os.WriteFile(outside, []byte("preserve me"), 0600); err != nil {
				t.Fatal(err)
			}
			child := filepath.Join(f.m.Root, testToken, "unexpected")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(child, 0700)
			case "symlink":
				err = os.Symlink(outside, child)
			case "file":
				err = os.WriteFile(child, nil, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = f.m.Cleanup(testToken); err == nil {
				t.Fatal("unexpected child accepted")
			}
			if _, err = os.Lstat(child); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(filepath.Join(f.m.Root, testToken, "executor")); err != nil {
				t.Fatal("partially removed staging")
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "preserve me" {
				t.Fatalf("outside modified: %q %v", data, err)
			}
		})
	}
}

func TestHardLinkedMetadataRefused(t *testing.T) {
	f := setup(t)
	f.prepare(t, testToken)
	original := filepath.Join(f.m.Root, testToken, "prepared.json")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Link(original, link); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Attach(context.Background(), testToken, nil, nil, nil); err == nil {
		t.Fatal("hard-linked record accepted")
	}
	if err := f.m.Cleanup(testToken); err == nil {
		t.Fatal("hard-linked record cleaned")
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatal(err)
	}
}

type foreignInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (i foreignInfo) Sys() any { return &i.stat }

func TestForeignOwnershipRejected(t *testing.T) {
	f := setup(t)
	f.prepare(t, testToken)
	for _, path := range []string{f.m.Root, filepath.Join(f.m.Root, testToken), filepath.Join(f.m.Root, testToken, "prepared.json")} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := *info.Sys().(*syscall.Stat_t)
		stat.Uid++
		if err := private(foreignInfo{info, stat}, info.IsDir(), info.Mode().Perm()); err == nil {
			t.Fatalf("foreign owner accepted: %s", path)
		}
	}
	// Exercise the full filesystem boundary where the test runner can chown.
	if os.Geteuid() != 0 {
		return
	}
	path := filepath.Join(f.m.Root, testToken, "prepared.json")
	if err := os.Chown(path, 1, -1); err != nil {
		t.Fatal(err)
	}
	defer os.Chown(path, 0, -1)
	if _, err := f.m.Attach(context.Background(), testToken, nil, nil, nil); err == nil {
		t.Fatal("foreign record attached")
	}
	if err := f.m.Cleanup(testToken); err == nil {
		t.Fatal("foreign record cleaned")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
