package ssh

import (
	"context"
	"errors"
	"fmt"

	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/firewall"
	"github.com/nchapman/vamos/internal/process"
)

// RunFirewall always uses a fresh SSH connection, including confirmation.
func RunFirewall(ctx context.Context, target string, request executor.Request, binary []byte) (*firewall.Status, error) {
	return runFirewall(ctx, target, request, binary, process.Run)
}

func runFirewall(ctx context.Context, target string, request executor.Request, binary []byte, run runProcess) (*firewall.Status, error) {
	if err := executor.ValidateFirewallRequest(request); err != nil {
		return nil, err
	}
	result, runErr := runStagedRequest(ctx, target, request, binary, run)
	var response executor.Response
	parseErr := decodeJSON(result.Stdout, &response)
	if parseErr == nil && response.Version == executor.Version && response.Error != nil && response.Firewall == nil && response.HostState == nil && response.Result == nil {
		return nil, fmt.Errorf("remote %s failed: %q", request.Operation, response.Error.Message)
	}
	if runErr != nil {
		return nil, fmt.Errorf("firewall outcome may be unknown; query server firewall status: %w", connectionError(target, result, runErr))
	}
	if parseErr != nil {
		return nil, fmt.Errorf("invalid firewall response: %w", parseErr)
	}
	if response.Version != executor.Version || response.Firewall == nil || response.Error != nil || response.HostState != nil || response.Result != nil {
		return nil, errors.New("invalid firewall response")
	}
	s := response.Firewall
	for _, port := range s.SSHPorts {
		if port < 1 || port > 65535 {
			return nil, errors.New("invalid firewall SSH allowance")
		}
	}
	switch request.Operation {
	case "firewall-enable":
		if !s.Enabled || s.Degraded || (s.Pending && s.Token == "") || (!s.Pending && s.Token != "") {
			return nil, errors.New("firewall enable did not return a usable enabled state")
		}
	case "firewall-confirm":
		if !s.Enabled || s.Pending || s.Degraded || s.Token != "" {
			return nil, errors.New("firewall confirmation did not return a confirmed enabled state")
		}
	case "firewall-disable":
		if s.Enabled || s.Pending || s.Degraded || s.Token != "" {
			return nil, errors.New("firewall disable did not return a disabled state")
		}
	}
	return s, nil
}
