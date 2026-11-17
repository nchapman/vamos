package deploy

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/engine"
	"github.com/nchapman/vamos/internal/process"
)

func TestRuntimeCommandRedaction(t *testing.T) {
	const secret = "FAKE_SECRET_password123"
	for _, tt := range []struct {
		stderr string
		code   string
	}{
		{"Error response from daemon: write /" + secret + ": no space left on device", "docker_disk_full"},
		{"invalid env " + secret + ": no space left on device", ""},
		{"invalid env\nCannot connect to the Docker daemon at " + secret + ". Is the docker daemon running?", ""},
	} {
		m := &Manager{Run: func(context.Context, string, []string, []byte) (process.Result, error) {
			return process.Result{Stdout: []byte(secret), Stderr: []byte(tt.stderr)}, errors.New(secret)
		}}
		out, err := m.cmd(context.Background(), "docker", "create", "image")
		if out != nil || err == nil || strings.Contains(err.Error(), secret) || ErrorCode(err) != tt.code {
			t.Fatalf("unsafe or incorrect result: %v", err)
		}
		// The engine's env-file path must retain the safe identity, with and without secrets.
		for _, values := range []map[string]string{nil, {"TOKEN": secret}} {
			c := m.engine()
			dir, pathErr := filepath.EvalSymlinks(t.TempDir())
			if pathErr != nil {
				t.Fatal(pathErr)
			}
			c.RuntimeDir = dir
			err = c.RunWithEnvFile(context.Background(), "image", []string{"create", "image"}, values)
			if err == nil || strings.Contains(err.Error(), secret) || ErrorCode(err) != tt.code {
				t.Fatalf("env-file result: %v", err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = m.cmd(ctx, "docker", "start", "container")
		if !errors.Is(err, context.Canceled) || ErrorCode(err) != "" {
			t.Fatal(err)
		}
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, engine.ErrDiskFull} {
		err := runtimeContext("create outcome unknown; no retry", cause)
		if !errors.Is(err, cause) {
			t.Fatal(err)
		}
	}
	if strings.Contains(runtimeContext("create failed", errors.New(secret)).Error(), secret) {
		t.Fatal("context leaked")
	}
}
