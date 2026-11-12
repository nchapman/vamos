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

	"github.com/nchapman/soar/internal/buildcontext"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/secrets"
)

const maxAppRequest = 64 * 1024

// ServeApp is a separate, bounded protocol so app configuration does not expand
// the host-lifecycle request surface. Only deploy reads the worker's fixed input.
func ServeApp(ctx context.Context, stdin io.Reader, stdout io.Writer) int {
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	request, err := readAppRequestContext(readCtx, stdin)
	cancel()
	if err != nil {
		return appFailure(stdout, err, 2)
	}
	if err := deploy.ValidateRequest(request); err != nil {
		return appFailure(stdout, err, 2)
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return appFailure(stdout, errors.New("app executor requires root on the bootstrapped Linux host"), 1)
	}
	var archive []byte
	if request.Action == "deploy" {
		info, err := os.Lstat("context.tar")
		if err != nil {
			return appFailure(stdout, err, 1)
		}
		if !info.Mode().IsRegular() || info.Size() > buildcontext.MaxBytes {
			return appFailure(stdout, errors.New("context.tar must be a regular file within the context size limit"), 1)
		}
		f, err := os.Open("context.tar")
		if err != nil {
			return appFailure(stdout, err, 1)
		}
		archive, err = readAppContext(f)
		f.Close()
		if err != nil {
			return appFailure(stdout, err, 1)
		}
	}
	if request.Action == "secrets-stage" {
		archive, err = readSecretFile("secret.value")
		if err != nil {
			return appFailure(stdout, errors.New("invalid secret input file"), 1)
		}
	}
	deadline := 20 * time.Minute
	if request.Action == "status" || request.Action == "logs" || request.Action == "secrets-list" {
		deadline = 15 * time.Second
	}
	ctx, stop := context.WithTimeout(ctx, deadline)
	defer stop()
	manager := deploy.NewManager()
	manager.RuntimeDir, err = os.Getwd()
	if err != nil {
		return appFailure(stdout, errors.New("worker directory unavailable"), 1)
	}
	response, err := manager.Execute(ctx, request, archive)
	if err != nil {
		return appFailure(stdout, err, 1)
	}
	// JSON escaping can expand each byte sixfold. Leave headroom under the
	// transport's 64 KiB per-stream limit even for control-heavy application logs.
	if len(response.Logs) > 8*1024 {
		response.Logs = "[log tail truncated to 8 KiB]\n" + response.Logs[len(response.Logs)-8*1024:]
	}
	if err := json.NewEncoder(stdout).Encode(response); err != nil {
		return 1
	}
	return 0
}

func readSecretFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > secrets.MaxValueBytes {
		return nil, errors.New("invalid secret file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, secrets.MaxValueBytes+1))
	if err != nil {
		return nil, errors.New("cannot read secret file")
	}
	if err := secrets.ValidateValue(data); err != nil {
		return nil, errors.New("invalid secret value")
	}
	return data, nil
}

func readAppContext(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, buildcontext.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > buildcontext.MaxBytes {
		return nil, errors.New("build context exceeds 32 MiB")
	}
	return data, nil
}

func appFailure(w io.Writer, err error, code int) int {
	message := err.Error()
	if len(message) > 4096 {
		message = message[:4096] + " [truncated]"
	}
	if writeErr := json.NewEncoder(w).Encode(deploy.Response{Version: 1, Error: message}); writeErr != nil {
		return 1
	}
	return code
}

func readAppRequestContext(ctx context.Context, input io.Reader) (deploy.Request, error) {
	type result struct {
		request deploy.Request
		err     error
	}
	results := make(chan result, 1)
	go func() { request, err := readAppRequest(input); results <- result{request, err} }()
	select {
	case r := <-results:
		return r.request, r.err
	case <-ctx.Done():
		if closer, ok := input.(io.Closer); ok {
			closer.Close()
		}
		return deploy.Request{}, fmt.Errorf("read app request: %w", ctx.Err())
	}
}

func readAppRequest(input io.Reader) (deploy.Request, error) {
	var request deploy.Request
	data, err := io.ReadAll(io.LimitReader(input, maxAppRequest+1))
	if err != nil {
		return request, err
	}
	if len(data) > maxAppRequest {
		return request, errors.New("app request exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return request, errors.New("expected exactly one app request")
	}
	return request, nil
}
