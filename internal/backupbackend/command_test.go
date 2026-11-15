package backupbackend

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOfficialCommandSignatures(t *testing.T) {
	for _, init := range []bool{false, true} {
		args, env := commandSpec(config(), creds(), "/private", init)
		want := "--no-cache --no-lock -o s3.bucket-lookup=path cat config"
		if init {
			want = "--no-cache --json -o s3.bucket-lookup=path init --repository-version 2"
		}
		if strings.Join(args, " ") != want {
			t.Fatal(args)
		}
		for _, v := range args {
			if strings.Contains(v, "synthetic") {
				t.Fatal("credential argv")
			}
		}
		for _, v := range env {
			key := strings.SplitN(v, "=", 2)[0]
			switch key {
			case "RESTIC_REPOSITORY_FILE", "RESTIC_PASSWORD_FILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_DEFAULT_REGION", "AWS_EC2_METADATA_DISABLED", "HOME", "TMPDIR":
			default:
				t.Fatal(key)
			}
		}
	}
}

// Opt-in Linux/root execution of the actual pinned download. The path must be
// pre-created root-owned 0700, with safe ancestors. Runtime scratch need not
// share the tool-store filesystem.
// This does NOT test S3 or initialize a repository.
func TestActualPinnedTool(t *testing.T) {
	root := os.Getenv("VAMOS_TEST_RESTIC_ROOT")
	if root == "" {
		t.Skip("set VAMOS_TEST_RESTIC_ROOT on disposable Linux root host")
	}
	if runtime.GOOS != "linux" {
		t.Fatal("Linux required")
	}
	if !safeDirectory(root, false) {
		t.Fatal("unsafe root")
	}
	scratch, e := os.MkdirTemp(root, "execution-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(scratch)
	b := New(root, scratch)
	tool, e := b.ensure(context.Background(), scratch)
	if e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{{"version"}, {"--no-cache", "--no-lock", "-o", "s3.bucket-lookup=path", "cat", "config", "--help"}, {"--no-cache", "--json", "-o", "s3.bucket-lookup=path", "init", "--repository-version", "2", "--help"}} {
		output, e := run(context.Background(), tool, args, []string{"HOME=" + scratch, "TMPDIR=" + scratch}, scratch)
		if e != nil || len(output) == 0 {
			t.Fatal(e)
		}
	}
	// A drifted pre-existing executable must not be repaired or overwritten.
	if e = os.WriteFile(tool, []byte("drift"), 0755); e != nil {
		t.Fatal(e)
	}
	if _, e = b.ensure(context.Background(), scratch); e == nil {
		t.Fatal("drift accepted")
	}
	if e = os.Remove(tool); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(filepath.Join(scratch, "missing"), tool); e != nil {
		t.Fatal(e)
	}
	if _, e = b.ensure(context.Background(), scratch); e == nil {
		t.Fatal("symlink accepted")
	}
}
