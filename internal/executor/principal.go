package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/nchapman/vamos/internal/bridgeacl"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

const MaxPrincipalRequest = 16 << 10
const MaxPrincipalOutput = process.OutputLimit

type PrincipalRequest struct {
	Version   int               `json:"version"`
	Operation string            `json:"operation"`
	Config    lifecycle.Config  `json:"config"`
	Principal bridgeacl.Request `json:"principal"`
}

func (r PrincipalRequest) Validate() error {
	if r.Version != Version || r.Operation != "principal" {
		return errors.New("invalid principal operation or protocol version")
	}
	if err := lifecycle.ValidateConfig(r.Config); err != nil {
		return err
	}
	return r.Principal.Validate()
}

type PrincipalResponse struct {
	Version int               `json:"version"`
	Entries []bridgeacl.Entry `json:"entries"`
	Error   *ProtocolError    `json:"error,omitempty"`
}

func runPrincipal(ctx context.Context, r PrincipalRequest, out io.Writer, collect host.Collect) int {
	if err := r.Validate(); err != nil {
		return failure(out, "invalid_request", err.Error(), 2)
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return failure(out, "principal_failed", "principal administration requires Linux root", 1)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// Hash the running image, not a pathname supplied by the client. Compare it
	// under the host lock in case an upgrade raced the client's artifact check.
	image, err := os.Open("/proc/self/exe")
	if err != nil {
		return failure(out, "principal_failed", "cannot open running executor", 1)
	}
	defer image.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, image); err != nil {
		return failure(out, "principal_failed", "cannot hash running executor", 1)
	}
	m := lifecycle.Manager{Root: "/", Run: lifecycle.RunCommand, Inspect: func(ctx context.Context) (*host.Report, error) { return host.Inspect(ctx, collect) }}
	var entries []bridgeacl.Entry
	err = m.WithPrincipalACL(ctx, r.Config, hex.EncodeToString(hash.Sum(nil)), func(dir string) error {
		var err error
		entries, err = bridgeacl.Apply(dir, r.Principal)
		return err
	})
	if err != nil {
		message := err.Error()
		if len(message) > 2048 {
			message = "principal operation failed; inspect the host ACL"
		}
		return failure(out, "principal_failed", message, 1)
	}
	if entries == nil {
		entries = []bridgeacl.Entry{}
	}
	data, err := json.Marshal(PrincipalResponse{Version: Version, Entries: entries})
	if err != nil || len(data)+1 > MaxPrincipalOutput {
		return failure(out, "principal_failed", "principal response exceeds output limit", 1)
	}
	if _, err := out.Write(append(data, '\n')); err != nil {
		return 1
	}
	return 0
}
