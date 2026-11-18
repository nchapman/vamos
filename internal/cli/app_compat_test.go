package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/deploy"
)

func TestLegacyProtectedRefusalDoesNotPrintUntargetedApproval(t *testing.T) {
	path, _, deps := appFixture(t)
	const legacy = "service web is protected and its configuration or staged secrets changed; explicitly deploy it with vamos deploy --service web --yes"
	deps.Agent = func(context.Context, string, deploy.Request, []byte) (deploy.Response, error) {
		return deploy.Response{Version: 1, Error: legacy}, errors.New(legacy)
	}
	var out, diag bytes.Buffer
	code := appCommand(context.Background(), "deploy", []string{"-f", path}, &out, &diag, deps)
	if code != 1 || strings.Contains(diag.String(), "--yes") || !strings.Contains(diag.String(), "Preview protected update:") || !strings.Contains(diag.String(), "--server home") {
		t.Fatalf("code=%d, diag=%s", code, &diag)
	}
}
