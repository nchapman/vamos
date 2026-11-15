package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/process"
)

type imageBuilderFunc func(context.Context, string, string, string, []byte) (process.Result, error)

func (f imageBuilderFunc) Build(ctx context.Context, hostID, dockerfile, iidfile string, archive []byte) (process.Result, error) {
	return f(ctx, hostID, dockerfile, iidfile, archive)
}

// The deployment tests simulate the external build service using their existing
// image-producing Docker fixture. Builder ownership, networking and lifecycle
// are exercised separately against that service's actual Docker boundary.
func fixtureBuilder(m *Manager) ImageBuilder {
	return imageBuilderFunc(func(ctx context.Context, hostID, dockerfile, iidfile string, archive []byte) (process.Result, error) {
		return m.Run(ctx, "docker", []string{"buildx", "build", "--load", "--network", "default", "--iidfile", iidfile, "--file", dockerfile, "-"}, archive)
	})
}

func TestBuildServiceReceivesCapturedInputs(t *testing.T) {
	m, _ := setup(t)
	want := errors.New("build service result")
	calls := 0
	m.Builder = imageBuilderFunc(func(_ context.Context, hostID, dockerfile, iidfile string, archive []byte) (process.Result, error) {
		calls++
		if hostID != strings.Repeat("a", 32) || dockerfile != "docker/Dockerfile" || iidfile != "/captured/image.iid" || string(archive) != "captured archive" {
			t.Fatal("build service did not receive the captured inputs")
		}
		return process.Result{Stderr: []byte("build diagnostic")}, want
	})
	out, err := m.buildImage(context.Background(), strings.Repeat("a", 32), "docker/Dockerfile", "/captured/image.iid", []byte("captured archive"))
	if !errors.Is(err, want) || calls != 1 || string(out.Stderr) != "build diagnostic" {
		t.Fatalf("build result lost: %v %v calls=%d", out, err, calls)
	}
}

func TestBuildServiceRefusesDaemonPolicyDrift(t *testing.T) {
	m, _ := setup(t)
	if err := atomic(m.path("/etc/docker/daemon.json"), []byte(`{"bridge":"none","log-driver":"local","live-restore":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	m.Builder = imageBuilderFunc(func(context.Context, string, string, string, []byte) (process.Result, error) {
		t.Fatal("started builder with unsupported daemon restart behavior")
		return process.Result{}, nil
	})
	if _, err := m.buildImage(context.Background(), strings.Repeat("a", 32), "Dockerfile", "/image.iid", []byte("archive")); err == nil {
		t.Fatal("accepted daemon policy drift")
	}
}

func TestBuilderSnapshotterPrefersManagerOverride(t *testing.T) {
	t.Setenv("VAMOS_BUILDKIT_SNAPSHOTTER", "auto")
	if got := builderSnapshotter(&Manager{Snapshotter: "native"}); got != "native" {
		t.Fatalf("snapshotter = %q, want manager override native", got)
	}
	if got := builderSnapshotter(&Manager{}); got != "auto" {
		t.Fatalf("snapshotter = %q, want environment fallback auto", got)
	}
}
