package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/backup"
)

func TestBackupHelpPaths(t *testing.T) {
	for _, path := range [][]string{{"backup"}, {"backup", "repository"}, {"backup", "repository", "add"}, {"backup", "repository", "resume"}} {
		args, ok := commandHelpArgs(path)
		if !ok || args[len(args)-1] != "--help" {
			t.Fatalf("help rejected %v", path)
		}
	}
	if _, ok := commandHelpArgs([]string{"backup", "repository", "capture"}); ok {
		t.Fatal("unsupported capture help accepted")
	}
}
func TestBackupPromptBoundedAndNoPrefetch(t *testing.T) {
	r := strings.NewReader("yes\nprivate")
	line, e := backupPromptLine(r)
	if e != nil || line != "yes" || r.Len() != 7 {
		t.Fatalf("line=%q err=%v remaining=%d", line, e, r.Len())
	}
	if _, e = backupPromptLine(strings.NewReader(strings.Repeat("x", 4097))); e == nil {
		t.Fatal("unbounded prompt")
	}
}
func TestBackupKitExclusiveAndUpdate(t *testing.T) {
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, "recovery.json")
	kit := backup.RecoveryKit{Name: "archive", Format: 2}
	kit.Credentials.Password = "original-key"
	staged, e := stageBackupKit(path, kit)
	if e != nil {
		t.Fatal(e)
	}
	defer staged.root.Close()
	if _, e = stageBackupKit(path, kit); e == nil {
		t.Fatal("overwrote kit")
	}
	info, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("unsafe permissions")
	}
	kit.RepositoryID = strings.Repeat("a", 64)
	if e = staged.update(kit); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var got backup.RecoveryKit
	if e = json.Unmarshal(data, &got); e != nil {
		t.Fatal(e)
	}
	if got.RepositoryID != kit.RepositoryID || got.Credentials.Password != "original-key" {
		t.Fatal("kit lost identity/key")
	}
	// Atomic replacement changes the inode; stale handles must not overwrite it.
	if e = staged.update(kit); e == nil {
		t.Fatal("accepted replaced inode")
	}
}
func TestBackupKitRejectsUnsafeParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode check")
	}
	dir := t.TempDir()
	if e := os.Chmod(dir, 0777); e != nil {
		t.Fatal(e)
	}
	defer os.Chmod(dir, 0700)
	if _, e := stageBackupKit(filepath.Join(dir, "kit"), backup.RecoveryKit{}); e == nil {
		t.Fatal("unsafe parent accepted")
	}
}

func TestBackupPrivateFieldsRejectNull(t *testing.T) {
	if backupPrivateFields([]byte(`{"access_key_id":"key","secret_access_key":"secret","session_token":null}`)) {
		t.Fatal("accepted null credential field")
	}
}
