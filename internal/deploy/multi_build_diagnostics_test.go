package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

func TestMultiBuildKeepsBoundedBuildDiagnostics(t *testing.T) {
	f := newMultiFlow(t)
	run := f.m.Run
	f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
		if name == "docker" && args[0] == "buildx" {
			return process.Result{Stderr: []byte(strings.Repeat("x", 8192) + "Dockerfile: unknown instruction")}, errors.New("exit status 1")
		}
		return run(ctx, name, args, input)
	}
	response, err := f.deploy(multiApp(), strings.Repeat("1", 32))
	if err == nil || !strings.Contains(response.Error, "Dockerfile: unknown instruction") || len(response.Error) > diagnosticLimit {
		t.Fatal("build diagnostics lost or unbounded", response.Error, err)
	}
}
