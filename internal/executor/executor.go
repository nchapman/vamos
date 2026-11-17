// Package executor provides the versioned stdin/stdout protocol for an installed
// host-side Vamos binary. Bootstrap SSH inspection does not require this binary.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/nchapman/vamos/internal/firewall"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

const Version = 1
const maxRequest = 4096

type Request struct {
	Version   int               `json:"version"`
	Operation string            `json:"operation"`
	Config    *lifecycle.Config `json:"config,omitempty"`
	Confirm   bool              `json:"confirm,omitempty"`
	Token     string            `json:"token,omitempty"`
	Purge     bool              `json:"purge,omitempty"`
	// DeployKey is one SSH public key line provisioned as the initial
	// restricted bridge key during bootstrap. It is never used by upgrade or
	// teardown, which preserve administrator-managed keys.
	DeployKey string `json:"deploy_key,omitempty"`
}

type Response struct {
	Firewall  *firewall.Status `json:"firewall,omitempty"`
	Version   int              `json:"version"`
	Result    *host.Report     `json:"result,omitempty"`
	HostState *lifecycle.State `json:"host_state,omitempty"`
	Error     *ProtocolError   `json:"error,omitempty"`
}

type ProtocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Serve handles exactly one request. Exit 0 means inspection completed (possibly
// unsupported), 1 means execution/output failure, and 2 means a bad request.
// stdout contains only a JSON response. Callers must close request stdin at EOF.
func Serve(ctx context.Context, stdin io.Reader, stdout io.Writer) int {
	return serve(ctx, stdin, stdout, collectLocal)
}

func serve(ctx context.Context, stdin io.Reader, stdout io.Writer, collect host.Collect) int {
	readCtx, cancelRead := context.WithTimeout(ctx, 15*time.Second)
	defer cancelRead()
	request, err := readRequestContext(readCtx, stdin)
	if err != nil {
		return failure(stdout, "invalid_request", err.Error(), 2)
	}
	if request.Version != Version {
		return failure(stdout, "unsupported_version", "supported protocol version is 1", 2)
	}
	if isFirewallOperation(request.Operation) {
		cancelRead()
		return runFirewall(ctx, request, stdout, collect)
	}
	if request.Operation == "bootstrap" || request.Operation == "upgrade" || request.Operation == "teardown" {
		cancelRead()
		return runLifecycle(ctx, request, stdout, collect)
	}
	if request.Operation != "inspect" {
		return failure(stdout, "unsupported_operation", "supported operations are inspect, bootstrap, upgrade, teardown, firewall-enable, firewall-confirm, firewall-disable, and firewall-status", 2)
	}
	if request.Config != nil || request.Confirm || request.Purge || request.Token != "" || request.DeployKey != "" {
		return failure(stdout, "invalid_request", "inspect does not accept mutation options", 2)
	}
	report, err := host.Inspect(readCtx, collect)
	if err != nil {
		return failure(stdout, "inspection_failed", err.Error(), 1)
	}
	if err := json.NewEncoder(stdout).Encode(Response{Version: Version, Result: report}); err != nil {
		return 1
	}
	return 0
}

func runLifecycle(ctx context.Context, request Request, stdout io.Writer, collect host.Collect) int {
	if request.Token != "" {
		return failure(stdout, "invalid_request", "only firewall-confirm accepts a token", 2)
	}
	if !request.Confirm || request.Config == nil {
		return failure(stdout, "invalid_request", "mutation requires explicit confirmation and server configuration", 2)
	}
	if err := lifecycle.ValidateConfig(*request.Config); err != nil {
		return failure(stdout, "invalid_request", err.Error(), 2)
	}
	if request.Operation == "teardown" && !request.Purge {
		return failure(stdout, "invalid_request", "teardown requires explicit data-purge authorization", 2)
	}
	if (request.Operation == "bootstrap" || request.Operation == "upgrade") && request.Purge {
		return failure(stdout, "invalid_request", "bootstrap and upgrade do not accept purge", 2)
	}
	if request.Operation != "bootstrap" && request.DeployKey != "" {
		return failure(stdout, "invalid_request", "only bootstrap provisions a deploy key", 2)
	}
	if request.DeployKey != "" {
		if err := lifecycle.ValidateDeployKey(request.DeployKey); err != nil {
			return failure(stdout, "invalid_request", err.Error(), 2)
		}
	}
	if runtime.GOOS != "linux" {
		return failure(stdout, "lifecycle_failed", "host lifecycle requires Linux", 1)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	manager := &lifecycle.Manager{Root: "/", Run: lifecycle.RunCommand, DeployKey: request.DeployKey, Inspect: func(ctx context.Context) (*host.Report, error) { return host.Inspect(ctx, collect) }}
	var state *lifecycle.State
	var err error
	if request.Operation == "bootstrap" || request.Operation == "upgrade" {
		filename, pathErr := os.Executable()
		if pathErr != nil {
			return failure(stdout, "lifecycle_failed", pathErr.Error(), 1)
		}
		manager.Executor, err = os.ReadFile(filename)
		if err == nil {
			if request.Operation == "bootstrap" {
				state, err = manager.Bootstrap(ctx, *request.Config)
			} else {
				state, err = manager.Upgrade(ctx, *request.Config)
			}
		}
	} else {
		state, err = manager.Teardown(ctx, request.Config.Name, request.Purge)
	}
	if err != nil {
		return failure(stdout, "lifecycle_failed", err.Error(), 1)
	}
	if err := json.NewEncoder(stdout).Encode(Response{Version: Version, HostState: state}); err != nil {
		return 1
	}
	return 0
}

func collectLocal(ctx context.Context) ([]byte, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("the host executor requires Linux; use server inspect to inspect a remote Ubuntu host")
	}
	result, err := process.Run(ctx, "/bin/sh", []string{"-s"}, []byte(host.Collector()))
	if err != nil {
		return nil, fmt.Errorf("local host probe: %w", err)
	}
	return result.Stdout, nil
}

// A peer that never closes stdin must not defeat cancellation. Production stdin
// is closable; arbitrary non-closable readers must eventually return themselves.
func readRequestContext(ctx context.Context, r io.Reader) (Request, error) {
	type result struct {
		request Request
		err     error
	}
	done := make(chan result, 1)
	go func() {
		request, err := readRequest(r)
		done <- result{request, err}
	}()
	select {
	case got := <-done:
		return got.request, got.err
	case <-ctx.Done():
		if closer, ok := r.(io.Closer); ok {
			closer.Close()
		}
		return Request{}, fmt.Errorf("read request: %w", ctx.Err())
	}
}

func readRequest(r io.Reader) (Request, error) {
	var request Request
	data, err := io.ReadAll(io.LimitReader(r, maxRequest+1))
	if err != nil {
		return request, fmt.Errorf("read request: %w", err)
	}
	if len(data) > maxRequest {
		return request, errors.New("request exceeds 4 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("decode request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return request, errors.New("expected exactly one JSON request")
	}
	return request, nil
}

func failure(w io.Writer, code, message string, exitCode int) int {
	if err := json.NewEncoder(w).Encode(Response{Version: Version, Error: &ProtocolError{Code: code, Message: message}}); err != nil {
		return 1
	}
	return exitCode
}
