package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nchapman/soar/internal/buildcontext"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/process"
)

//go:embed app.sh
var appScript string

//go:embed app-read.sh
var appReadScript string

func RunApp(ctx context.Context, target string, request deploy.Request, binary, archive []byte) (deploy.Response, error) {
	return runApp(ctx, target, request, binary, archive, process.Run)
}

func runApp(ctx context.Context, target string, request deploy.Request, binary, archive []byte, run runProcess) (deploy.Response, error) {
	var response deploy.Response
	if err := ValidateTarget(target); err != nil {
		return response, err
	}
	if err := deploy.ValidateRequest(request); err != nil {
		return response, err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return response, err
	}
	if len(encoded) > 64*1024 {
		return response, errors.New("app request exceeds 64 KiB")
	}
	mutation := request.Action == "deploy" || request.Action == "resume" || request.Action == "cleanup" || request.Action == "destroy"
	payload, err := appArchive(encoded, binary, archive, request.Action == "deploy")
	if err != nil {
		return response, err
	}
	script := appReadScript
	if mutation {
		script = appScript
	}
	command := "/bin/sh -c '" + strings.ReplaceAll(script, "'", "'\\''") + "'"
	result, runErr := run(ctx, "ssh", arguments(target, command), payload)
	decodeErr := decodeJSON(result.Stdout, &response)
	if decodeErr == nil && response.Version == 1 && response.Error != "" {
		return response, fmt.Errorf("remote %s failed: %q", request.Action, response.Error)
	}
	if runErr != nil {
		err := connectionError(target, result, runErr)
		if mutation {
			return response, fmt.Errorf("deployment outcome may be unknown; use soar status before retrying: %w", err)
		}
		return response, err
	}
	if decodeErr != nil || response.Version != 1 || (request.Action != "logs" && response.Status == nil && response.Plan == nil) {
		return response, fmt.Errorf("invalid app response; inspect status before retrying (decode error: %v)", decodeErr)
	}
	return response, nil
}

func appArchive(request, binary, contextTar []byte, needsContext bool) ([]byte, error) {
	if len(binary) == 0 || len(binary) > 64*1024*1024 {
		return nil, errors.New("invalid executor artifact size")
	}
	if len(contextTar) > buildcontext.MaxBytes || (needsContext && len(contextTar) == 0) {
		return nil, errors.New("missing or oversized build context")
	}
	var buffer bytes.Buffer
	tw := tar.NewWriter(&buffer)
	hash := sha256.Sum256(binary)
	for _, file := range []struct {
		name string
		data []byte
		mode int64
	}{{"executor", binary, 0700}, {"request.json", request, 0600}, {"context.tar", contextTar, 0600}, {"checksum", []byte(fmt.Sprintf("%x  executor\n", hash)), 0600}} {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(file.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
