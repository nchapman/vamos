package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
)

func isFirewallOperation(op string) bool {
	switch op {
	case "firewall-enable", "firewall-confirm", "firewall-disable", "firewall-status":
		return true
	}
	return false
}

// ValidateFirewallRequest is shared with the SSH client to reject inappropriate
// authorization and tokens before opening a connection.
func ValidateFirewallRequest(r Request) error {
	if r.Version != Version || !isFirewallOperation(r.Operation) || r.Config == nil {
		return errors.New("firewall requires a supported operation, protocol version and server configuration")
	}
	if err := lifecycle.ValidateConfig(*r.Config); err != nil {
		return err
	}
	if r.Purge || r.DeployKey != "" {
		return errors.New("firewall does not accept purge or deploy-key")
	}
	if r.Confirm != (r.Operation != "firewall-status") {
		return errors.New("firewall mutations require explicit confirmation; status is read-only")
	}
	if (r.Operation == "firewall-confirm") != (r.Token != "") {
		return errors.New("only firewall-confirm requires and accepts a token")
	}
	return nil
}

func runFirewall(ctx context.Context, r Request, out io.Writer, collect host.Collect) int {
	if err := ValidateFirewallRequest(r); err != nil {
		return failure(out, "invalid_request", err.Error(), 2)
	}
	if runtime.GOOS != "linux" {
		return failure(out, "firewall_failed", "managed firewall requires Linux", 1)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	manager := &lifecycle.Manager{Root: "/", Run: lifecycle.RunCommand, Inspect: func(ctx context.Context) (*host.Report, error) { return host.Inspect(ctx, collect) }}
	status, err := manager.Firewall(ctx, *r.Config, strings.TrimPrefix(r.Operation, "firewall-"), r.Token)
	if err != nil && !(r.Operation == "firewall-status" && status.Degraded) {
		return failure(out, "firewall_failed", err.Error(), 1)
	}
	// Status is an observation: retain structured degradation details even when
	// verification fails. Mutations must still fail rather than claim success.
	if err := json.NewEncoder(out).Encode(Response{Version: Version, Firewall: &status}); err != nil {
		return 1
	}
	return 0
}
