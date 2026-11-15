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

	"github.com/nchapman/vamos/internal/buildcontext"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/process"
	"github.com/nchapman/vamos/internal/secrets"
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
	if request.Action == "deploy" || request.Action == "run" {
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
	if request.Action == "secrets-stage" || request.Action == "secrets-set" {
		archive, err = readSecretsFile("secret.value", request)
		if err != nil {
			return appFailure(stdout, errors.New("invalid secret input file"), 1)
		}
	}
	if request.Action == "backup-repository" && request.Backup.NeedsCredentials() {
		archive, err = readRepositoryFile("repository.credentials")
		if err != nil {
			return appFailure(stdout, errors.New("invalid private repository input file"), 1)
		}
	}
	deadline := 20 * time.Minute
	if request.Action == "apps" || request.Action == "status" || request.Action == "logs" || request.Action == "secrets-list" || ((request.Action == "backup-repository" || request.Action == "backup-plan") && !request.Mutating()) {
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
	formatted := FormatAppResponse(request, response, err)
	data, marshalErr := json.Marshal(formatted)
	if marshalErr != nil {
		return appFailure(stdout, marshalErr, 1)
	}
	if _, writeErr := stdout.Write(append(data, '\n')); writeErr != nil {
		return 1
	}
	if err != nil || formatted.Error != "" {
		return 1
	}
	return 0
}

// FormatAppResponse normalizes an engine result into a bounded protocol
// response, redacting secret-operation errors and enforcing the 64 KiB wire cap.
func FormatAppResponse(request deploy.Request, response deploy.Response, runErr error) deploy.Response {
	if runErr != nil && (request.Action == "secrets-stage" || request.Action == "secrets-set" || request.Action == "secrets-list") {
		message := deploy.RefusalMessage(request.Action, deploy.ErrorCode(runErr))
		if message == "" {
			message = "secret request outcome unknown"
		}
		return deploy.Response{Version: 1, Error: message, ErrorCode: deploy.ErrorCode(runErr)}
	}
	response.Version = 1
	if runErr != nil {
		message := runErr.Error()
		if len(message) > 4096 {
			message = message[:4096] + " [truncated]"
		}
		response.Error = message
		response.ErrorCode = deploy.ErrorCode(runErr)
	}
	boundResponseOutputs(&response)
	data, err := json.Marshal(response)
	if err != nil {
		return fallbackFailureResponse(err)
	}
	if len(data)+1 > process.OutputLimit {
		if response.Error != "" {
			return deploy.Response{Version: 1, Error: response.Error, ErrorCode: response.ErrorCode}
		}
		return deploy.Response{
			Version: 1,
			Error:   "app response exceeds 64 KiB; request status or logs with --service NAME, or secrets list separately",
		}
	}
	return response
}

func boundResponseOutputs(response *deploy.Response) {
	boundRun := func(r *deploy.RunResult) {
		if r != nil && len(r.Output) > 8*1024 {
			r.Output = "[run output truncated to 8 KiB]\n" + r.Output[len(r.Output)-8*1024:]
		}
	}
	boundRun(response.Run)
	if response.Status != nil {
		boundRun(response.Status.Run)
		boundRun(response.Status.Release)
	}
	// JSON escaping can expand each byte sixfold. Leave headroom under the
	// transport's 64 KiB per-stream limit even for control-heavy application logs.
	if len(response.Logs) > 8*1024 {
		response.Logs = "[log tail truncated to 8 KiB]\n" + response.Logs[len(response.Logs)-8*1024:]
	}
}

func fallbackFailureResponse(err error) deploy.Response {
	message := err.Error()
	if len(message) > 4096 {
		message = message[:4096] + " [truncated]"
	}
	return deploy.Response{Version: 1, Error: message, ErrorCode: deploy.ErrorCode(err)}
}

func writeAppResponse(stdout io.Writer, response deploy.Response) int {
	formatted := FormatAppResponse(deploy.Request{}, response, nil)
	data, err := json.Marshal(formatted)
	if err != nil {
		return appFailure(stdout, err, 1)
	}
	if _, err = stdout.Write(append(data, '\n')); err != nil {
		return 1
	}
	if formatted.Error != "" {
		return 1
	}
	return 0
}

func readPrivateSecretFile(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxBytes {
		return nil, errors.New("invalid secret file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, errors.New("cannot read secret file")
	}
	if int64(len(data)) > maxBytes {
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
	if writeErr := json.NewEncoder(w).Encode(deploy.Response{Version: 1, Error: message, ErrorCode: deploy.ErrorCode(err)}); writeErr != nil {
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

func readSecretFile(path string) ([]byte, error) {
	data, err := readPrivateSecretFile(path, secrets.MaxValueBytes)
	if err != nil {
		return nil, err
	}
	if err := secrets.ValidateValue(data); err != nil {
		return nil, errors.New("invalid secret value")
	}
	return data, nil
}
func readSecretsFile(path string, r deploy.Request) ([]byte, error) {
	maxBytes := int64(secrets.MaxValueBytes)
	if len(r.SecretNames) > 0 {
		maxBytes = secrets.MaxBatchBytes
	}
	data, err := readPrivateSecretFile(path, maxBytes)
	if err != nil {
		return nil, err
	}
	if _, err := deploy.SecretsInput(r, data); err != nil {
		return nil, errors.New("invalid secret input")
	}
	return data, nil
}
