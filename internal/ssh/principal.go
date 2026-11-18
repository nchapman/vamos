package ssh

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/nchapman/vamos/internal/appstate"
	"github.com/nchapman/vamos/internal/bridgeacl"
	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

// RunPrincipal stages the bundled host executor via a fresh admin SSH connection.
func RunPrincipal(ctx context.Context, target string, cfg lifecycle.Config, req bridgeacl.Request, binary []byte) ([]bridgeacl.Entry, error) {
	return runPrincipal(ctx, target, cfg, req, binary, process.Run)
}

func runPrincipal(ctx context.Context, target string, cfg lifecycle.Config, req bridgeacl.Request, binary []byte, run runProcess) ([]bridgeacl.Entry, error) {
	request := executor.PrincipalRequest{Version: executor.Version, Operation: "principal", Config: cfg, Principal: req}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	result, runErr := runStagedAdminRequest(ctx, target, request, binary, executor.MaxPrincipalRequest, run)
	reply, parseErr := executor.DecodePrincipalResponse(result.Stdout)
	if parseErr == nil && reply.Version == executor.Version && reply.Error != nil && reply.Entries == nil && reply.Error.Code != "" && reply.Error.Message != "" {
		return nil, fmt.Errorf("remote principal administration failed: %q", reply.Error.Message)
	}
	if runErr != nil {
		return nil, fmt.Errorf("principal outcome may be unknown; check server principal list: %w", connectionError(target, result, runErr))
	}
	if parseErr != nil || reply.Version != executor.Version || reply.Error != nil || reply.Entries == nil {
		return nil, errors.New("invalid principal response")
	}
	if req.Action != "list" && len(reply.Entries) != 0 {
		return nil, errors.New("unexpected entries in principal mutation response")
	}
	seen := map[string]bool{}
	for _, e := range reply.Entries {
		fp, ok := strings.CutPrefix(e.Fingerprint, "SHA256:")
		digest, err := base64.RawStdEncoding.Strict().DecodeString(fp)
		probe := bridgeacl.Request{Action: "revoke", Name: e.Name}
		if probe.Validate() != nil || !appstate.ValidName(e.App) || !ok || err != nil || len(digest) != 32 || base64.RawStdEncoding.EncodeToString(digest) != fp || seen[e.Name] {
			return nil, errors.New("invalid principal entry")
		}
		seen[e.Name] = true
	}
	return reply.Entries, nil
}
