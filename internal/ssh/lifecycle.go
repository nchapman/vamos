package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nchapman/soar/internal/executor"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/process"
)

//go:embed bootstrap.sh
var bootstrapScript string

const statusScript = `set -eu
for p in /var/lib/soar /var/lib/soar/host.json; do
  if [ -L "$p" ]; then printf '%s\n' 'refusing symlinked host state' >&2; exit 1; fi
done
active=false
if systemctl is-active --quiet soar-host-operation.service; then active=true; fi
printf '{"active":%s,"state":' "$active"
if [ -f /var/lib/soar/host.json ]; then
  head -c 1048577 /var/lib/soar/host.json
else
  printf 'null'
fi
printf '}\n'
`

type Status struct {
	Active bool             `json:"active"`
	State  *lifecycle.State `json:"state"`
}

func HostStatus(ctx context.Context, target string) (*Status, error) {
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}
	result, err := process.Run(ctx, "ssh", arguments(target, "/bin/sh -s"), []byte(statusScript))
	if err != nil {
		return nil, connectionError(target, result, err)
	}
	var status Status
	if err := decodeJSON(result.Stdout, &status); err != nil {
		return nil, fmt.Errorf("invalid host status: %w", err)
	}
	if status.State != nil && status.State.Version != 1 {
		return nil, errors.New("unsupported host state version")
	}
	return &status, nil
}

// Artifact selects a sibling Linux artifact from the development/release bundle.
// An explicit path is useful when recovering an installation with an older build.
func Artifact(override, arch string) ([]byte, error) {
	filename := override
	if filename == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		filename = filepath.Join(filepath.Dir(self), "soar-linux-"+arch)
	}
	info, err := os.Stat(filename)
	if err != nil {
		return nil, fmt.Errorf("Linux executor artifact missing: %w; run mise run build or supply --executor", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64*1024*1024 {
		return nil, errors.New("executor artifact must be a regular file at most 64 MiB")
	}
	file, err := elf.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("executor must be a Linux ELF binary: %w", err)
	}
	defer file.Close()
	if (arch == "amd64" && file.Machine != elf.EM_X86_64) || (arch == "arm64" && file.Machine != elf.EM_AARCH64) || (arch != "amd64" && arch != "arm64") {
		return nil, errors.New("executor architecture does not match inspected host")
	}
	return os.ReadFile(filename)
}

func RunLifecycle(ctx context.Context, target string, request executor.Request, binary []byte) (*lifecycle.State, error) {
	return runLifecycle(ctx, target, request, binary, process.Run)
}

func runLifecycle(ctx context.Context, target string, request executor.Request, binary []byte, run runProcess) (*lifecycle.State, error) {
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}
	payload, err := archiveRequest(request, binary)
	if err != nil {
		return nil, err
	}
	// This command is assembled only from an embedded constant, never request fields.
	command := "/bin/sh -c '" + strings.ReplaceAll(bootstrapScript, "'", "'\\''") + "'"
	result, runErr := run(ctx, "ssh", arguments(target, command), payload)
	var response executor.Response
	parseErr := decodeJSON(result.Stdout, &response)
	if parseErr == nil && response.Version == executor.Version && response.Error != nil {
		return nil, fmt.Errorf("remote %s failed: %q; inspect server status and retry the same operation after resolving the cause", request.Operation, response.Error.Message)
	}
	if runErr != nil {
		return nil, fmt.Errorf("operation outcome may be unknown; query server status before retrying: %w", connectionError(target, result, runErr))
	}
	if parseErr != nil || response.Version != executor.Version || response.HostState == nil {
		return nil, fmt.Errorf("invalid lifecycle response; query server status before retrying (decode error: %v)", parseErr)
	}
	return response.HostState, nil
}

func archiveRequest(request executor.Request, binary []byte) ([]byte, error) {
	if len(binary) == 0 || len(binary) > 64*1024*1024 {
		return nil, errors.New("invalid executor artifact size")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 4096 {
		return nil, errors.New("request exceeds 4 KiB")
	}
	hash := sha256.Sum256(binary)
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, file := range []struct {
		name string
		data []byte
		mode int64
	}{{"executor", binary, 0700}, {"request.json", encoded, 0600}, {"checksum", []byte(fmt.Sprintf("%x  executor\n", hash)), 0600}} {
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
	return archive.Bytes(), nil
}

func decodeJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("expected exactly one JSON response")
	}
	return nil
}
