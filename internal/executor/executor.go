// Package executor provides the versioned stdin/stdout protocol for an installed
// host-side Soar binary. Bootstrap SSH inspection does not require this binary.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/nchapman/soar/internal/host"
	"github.com/nchapman/soar/internal/process"
)

const Version = 1
const maxRequest = 4096

type Request struct {
	Version   int    `json:"version"`
	Operation string `json:"operation"`
}

type Response struct {
	Version int            `json:"version"`
	Result  *host.Report   `json:"result,omitempty"`
	Error   *ProtocolError `json:"error,omitempty"`
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
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := readRequestContext(ctx, stdin)
	if err != nil {
		return failure(stdout, "invalid_request", err.Error(), 2)
	}
	if request.Version != Version {
		return failure(stdout, "unsupported_version", "supported protocol version is 1", 2)
	}
	if request.Operation != "inspect" {
		return failure(stdout, "unsupported_operation", "only inspect is implemented", 2)
	}
	report, err := host.Inspect(ctx, collect)
	if err != nil {
		return failure(stdout, "inspection_failed", err.Error(), 1)
	}
	if err := json.NewEncoder(stdout).Encode(Response{Version: Version, Result: report}); err != nil {
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
