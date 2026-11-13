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
	"github.com/nchapman/soar/internal/secrets"
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
	mutation := request.Mutating()
	payload, err := actionArchive(encoded, binary, archive, request.Action)
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
	if request.Action == "secrets-stage" {
		if runErr != nil || decodeErr != nil || response.Version != 1 || response.Error != "" || response.Secret == nil {
			return deploy.Response{}, errors.New("secret staging outcome unknown; inspect retained versions with soar secrets list")
		}
		return response, nil
	}
	// A decoder can populate fields before rejecting trailing data or an
	// unsupported version. Such fields cannot acknowledge a local binding.
	if decodeErr != nil || response.Version != 1 {
		response = deploy.Response{}
	}
	if decodeErr == nil && response.Version == 1 && request.Action == "run" && response.Run != nil && response.Run.ExitCode != nil {
		return response, nil
	}
	if decodeErr == nil && response.Version == 1 && response.Error != "" {
		return response, fmt.Errorf("remote %s failed: %q", request.Action, response.Error)
	}
	if request.Action == "run" && (runErr != nil || decodeErr != nil || response.Version != 1 || response.Run == nil) {
		return response, fmt.Errorf("run outcome unknown; inspect soar status --run %s", request.Operation)
	}
	if runErr != nil {
		err := connectionError(target, result, runErr)
		if mutation {
			return response, fmt.Errorf("deployment outcome may be unknown; use soar status before retrying: %w", err)
		}
		return response, err
	}
	if decodeErr != nil || response.Version != 1 || (request.Action != "logs" && response.Status == nil && response.Plan == nil && response.Run == nil && response.DeployPreview == nil && response.RollbackPreview == nil) {
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
	return payloadArchive(request, binary, contextTar, "context.tar")
}

func actionArchive(request, binary, input []byte, action string) ([]byte, error) {
	if len(binary) == 0 || len(binary) > 64*1024*1024 {
		return nil, errors.New("invalid executor artifact size")
	}
	switch action {
	case "deploy", "run":
		return appArchive(request, binary, input, true)
	case "secrets-stage":
		if err := secrets.ValidateValue(input); err != nil {
			return nil, errors.New("invalid secret input")
		}
		return payloadArchive(request, binary, input, "secret.value")
	default:
		if len(input) != 0 {
			return nil, errors.New("action does not accept input")
		}
		return payloadArchive(request, binary, nil, "")
	}
}

func payloadArchive(request, binary, input []byte, inputName string) ([]byte, error) {
	var buffer bytes.Buffer
	tw := tar.NewWriter(&buffer)
	hash := sha256.Sum256(binary)
	for _, file := range []struct {
		name string
		data []byte
		mode int64
	}{{"executor", binary, 0700}, {"request.json", request, 0600}, {inputName, input, 0600}, {"checksum", []byte(fmt.Sprintf("%x  executor\n", hash)), 0600}} {
		if file.name == "" {
			continue
		}
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
