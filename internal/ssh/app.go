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

	"github.com/nchapman/vamos/internal/backup"
	"github.com/nchapman/vamos/internal/buildcontext"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/process"
	"github.com/nchapman/vamos/internal/secrets"
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
	if request.Action == "backup-repository" {
		if request.Backup.NeedsCredentials() {
			if _, err := backup.DecodeCredentials(archive); err != nil {
				return response, errors.New("invalid private repository credentials")
			}
		} else if len(archive) != 0 {
			return response, errors.New("repository action does not accept credentials")
		}
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
	var connErr error
	if runErr != nil {
		connErr = connectionError(target, result, runErr)
	}
	return appOutcome(request, response, decodeErr, connErr)
}

// appOutcome maps one decoded executor response to the CLI's strict result
// semantics. decodeErr is set when the envelope is untrusted or incomplete;
// connErr carries a transport failure. Both transports (uploaded executor and
// installed agent) share these refusal and acknowledgment rules.
func appOutcome(request deploy.Request, response deploy.Response, decodeErr, connErr error) (deploy.Response, error) {
	runErr := connErr
	mutation := request.Mutating()
	if request.Action == "backup-plan" {
		if decodeErr == nil && response.Version == 1 && response.Error != "" {
			if message := deploy.RefusalMessage(request.Action, response.ErrorCode); message != "" {
				return deploy.Response{Version: 1, Error: message, ErrorCode: response.ErrorCode}, errors.New(message)
			}
		}
		if runErr != nil || decodeErr != nil || response.Version != 1 || response.Error != "" || response.ErrorCode != "" || response.BackupPlan == nil {
			return deploy.Response{}, errors.New("backup plan request outcome unknown; inspect recorded policy before retrying")
		}
		return deploy.Response{Version: 1, BackupPlan: response.BackupPlan}, nil
	}
	if request.Action == "backup-repository" {
		if decodeErr == nil && response.Version == 1 && response.Error != "" {
			if message := deploy.RefusalMessage(request.Action, response.ErrorCode); message != "" {
				return deploy.Response{Version: 1, Error: message, ErrorCode: response.ErrorCode}, errors.New(message)
			}
		}
		if runErr != nil || decodeErr != nil || response.Version != 1 || response.Error != "" || response.ErrorCode != "" || response.Backup == nil {
			return deploy.Response{}, errors.New("repository request outcome unknown; inspect its recorded state before retrying")
		}
		// Raw remote logs and unrelated response fields cannot escape through the
		// repository command, including on malformed/failed SSH responses.
		return deploy.Response{Version: 1, Backup: response.Backup}, nil
	}
	if request.Action == "secrets-stage" || request.Action == "secrets-set" || request.Action == "secrets-list" {
		if decodeErr == nil && response.Version == 1 && response.Error != "" {
			if message := deploy.RefusalMessage(request.Action, response.ErrorCode); message != "" {
				return deploy.Response{Version: 1, ErrorCode: response.ErrorCode, Error: message}, errors.New(message)
			}
		}
		if request.Action == "secrets-list" {
			if runErr != nil || decodeErr != nil || response.Version != 1 || response.Error != "" || response.ErrorCode != "" {
				return deploy.Response{}, errors.New("secret metadata request failed")
			}
			// Multi-service metadata responses intentionally omit Status. An
			// empty inventory also omits SecretVersions via its JSON omitempty.
			return response, nil
		}
	}
	if request.Action == "secrets-stage" || request.Action == "secrets-set" {
		if runErr != nil || decodeErr != nil || response.Version != 1 || response.Error != "" || response.ErrorCode != "" || !validSecretResponse(request, response) {
			return deploy.Response{}, errors.New("secret staging outcome unknown; inspect retained versions with vamos secrets list")
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
		return response, fmt.Errorf("run outcome unknown; inspect vamos status --run %s", request.Operation)
	}
	if runErr != nil {
		err := runErr
		if mutation {
			return response, fmt.Errorf("deployment outcome may be unknown; use vamos status before retrying: %w", err)
		}
		return response, err
	}
	if decodeErr != nil || response.Version != 1 || (request.Action == "apps" && response.Apps == nil) || (request.Action != "apps" && request.Action != "logs" && response.Status == nil && response.Plan == nil && response.Run == nil && response.DeployPreview == nil && response.RollbackPreview == nil) {
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
	case "backup-repository":
		if len(input) == 0 {
			return payloadArchive(request, binary, nil, "")
		}
		if _, err := backup.DecodeCredentials(input); err != nil {
			return nil, errors.New("invalid private repository credentials")
		}
		return payloadArchive(request, binary, input, "repository.credentials")
	case "secrets-stage", "secrets-set":
		var r deploy.Request
		if err := json.Unmarshal(request, &r); err != nil {
			return nil, errors.New("invalid secret request")
		}
		err := secrets.ValidateValue(input)
		if len(r.SecretNames) != 0 {
			_, err = secrets.DecodeBatch(input, r.SecretNames)
		}
		if err != nil {
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

// validSecretResponse requires an exact acknowledgement of every requested version.
func validSecretResponse(r deploy.Request, response deploy.Response) bool {
	if r.Action == "secrets-set" && !response.SetApplied {
		return false
	}
	if len(r.SecretNames) == 0 {
		return response.Secret != nil && len(response.SecretVersions) == 0 && response.Secret.Name == r.SecretName && response.Secret.Version == r.Operation
	}
	if response.Secret != nil || len(response.SecretVersions) != len(r.SecretNames) {
		return false
	}
	expected := make(map[string]string, len(r.SecretNames))
	for _, name := range r.SecretNames {
		expected[name] = secrets.VersionFor(r.Operation, name)
	}
	for _, entry := range response.SecretVersions {
		version, ok := expected[entry.Name]
		if !ok || entry.Version != version {
			return false
		}
		delete(expected, entry.Name)
	}
	return len(expected) == 0
}
