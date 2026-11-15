package cli

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

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/process"
	"github.com/nchapman/vamos/internal/session"
)

func internalSession(ctx context.Context, action string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return usageError(stderr, "internal session command requires exactly one session token")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return lifecycleError(stderr, errors.New("session executor requires Linux root"))
	}
	manager := session.NewManager()
	switch action {
	case "internal-session-prepare":
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		request, err := decodeSessionRequest(stdin)
		if err != nil {
			return writeSessionPreparation(stdout, session.Preparation{Version: 1}, err)
		}
		executable, err := os.Executable()
		if err != nil {
			return writeSessionPreparation(stdout, session.Preparation{Version: 1}, err)
		}
		prepared, err := manager.Prepare(ctx, args[0], request, executable)
		return writeSessionPreparation(stdout, prepared, err)
	case "internal-session-attach":
		code, err := manager.Attach(ctx, args[0], stdin, stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "session failed: %v\n", err)
			return 125
		}
		if code < 0 || code > 255 {
			return 125
		}
		return code
	case "internal-session-cleanup":
		if err := manager.Cleanup(args[0]); err != nil {
			return lifecycleError(stderr, err)
		}
		return 0
	default:
		return usageError(stderr, "unsupported internal session command")
	}
}

func decodeSessionRequest(reader io.Reader) (deploy.SessionRequest, error) {
	var request deploy.SessionRequest
	data, err := io.ReadAll(io.LimitReader(reader, process.OutputLimit+1))
	if err != nil || len(data) > process.OutputLimit {
		return request, errors.New("invalid or oversized session request")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, errors.New("invalid session request JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return request, errors.New("trailing session request data")
	}
	return request, deploy.ValidateSessionRequest(request)
}

func writeSessionPreparation(stdout io.Writer, prepared session.Preparation, err error) int {
	prepared.Version = 1
	if err != nil {
		prepared.Error = err.Error()
		if len(prepared.Error) > 4096 {
			prepared.Error = "session preparation failed"
		}
	}
	if encodeErr := json.NewEncoder(stdout).Encode(prepared); encodeErr != nil || err != nil {
		return 1
	}
	return 0
}
