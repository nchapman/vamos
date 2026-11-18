package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
)

// Host administration uses the installed bundle, never an implicit upgrade.
func matchingAdminArtifact(ctx context.Context, deps lifecycleDeps, target string, cfg lifecycle.Config, path string) ([]byte, error) {
	status, err := deps.Status(ctx, target)
	if err != nil {
		return nil, err
	}
	if status == nil || status.Active || status.State == nil || status.State.Config != cfg || status.State.Phase != "ready" {
		return nil, errors.New("operation requires an idle ready host with matching ownership identity")
	}
	inspectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	report, err := deps.Inspect(inspectCtx, target)
	if err != nil {
		return nil, err
	}
	if err := host.RequireAdmin(report); err != nil {
		return nil, err
	}
	binary, err := deps.Artifact(path, report.Architecture)
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(binary)) != status.State.ExecutorSHA {
		return nil, errors.New("executor artifact does not match the installed host bundle; use a matching --executor or server upgrade")
	}
	return binary, nil
}
