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

	"github.com/nchapman/vamos/internal/buildmeta"
	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

//go:embed bootstrap.sh
var bootstrapScript string

const statusScript = `set -eu
for p in /var/lib/vamos /var/lib/vamos/host.json /var/lib/vamos/queue /var/lib/vamos/queue/operations; do
  if [ -L "$p" ]; then printf '%s\n' 'refusing symlinked host state' >&2; exit 1; fi
done
# A failed query is not evidence that the operation is at rest. Capture both
# the state and exit code, suppressing systemctl's potentially sensitive errors.
check_unit() {
  code=0
  service_state=$(systemctl is-active "$1" 2>/dev/null) || code=$?
  case "$service_state:$code" in
    active:0|reloading:0|activating:0|activating:3|deactivating:0|deactivating:3)
      return 0 ;;
    inactive:3|inactive:4|failed:3|unknown:3|unknown:4)
      # Missing units report inactive:4 on systemd 255 or unknown on older versions.
      return 1 ;;
    *)
      printf '%s\n' 'cannot establish host operation service state' >&2
      exit 1 ;;
  esac
}
active=false
if check_unit vamos-host-operation.service; then
  active=true
elif [ -d /var/lib/vamos/queue/operations ]; then
  for rec in /var/lib/vamos/queue/operations/*.json; do
    [ -e "$rec" ] || break
    if [ -L "$rec" ] || [ ! -f "$rec" ]; then
      printf '%s\n' 'refusing invalid queue record' >&2
      exit 1
    fi
    op_id=${rec##*/}
    op_id=${op_id%.json}
    if check_unit "vamos-op-${op_id}.service"; then
      active=true
      break
    fi
    if grep -Eq '"phase"[[:space:]]*:[[:space:]]*"(queued|running)"' "$rec"; then
      active=true
      break
    fi
  done
fi
printf '{"active":%s,"state":' "$active"
if [ -f /var/lib/vamos/host.json ]; then
  head -c 1048577 /var/lib/vamos/host.json
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
	result, err := process.Run(ctx, "ssh", arguments(target, adminCommand("/bin/sh -s")), []byte(statusScript))
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
	return artifactForBuild(override, arch, buildmeta.Stamp)
}

func artifactForBuild(override, arch, stamp string) ([]byte, error) {
	filename := override
	if filename == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		filename = filepath.Join(filepath.Dir(self), "vamos-linux-"+arch)
	}
	input, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("Linux executor artifact missing: %w; run mise run build or supply --executor", err)
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return nil, fmt.Errorf("Linux executor artifact missing: %w; run mise run build or supply --executor", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64*1024*1024 {
		return nil, errors.New("executor artifact must be a regular file at most 64 MiB")
	}
	// Validate the same bounded bytes that will be uploaded, not a second
	// pathname read that could race with publication of a new bundle.
	data, err := io.ReadAll(io.LimitReader(input, 64*1024*1024+1))
	if err != nil || len(data) > 64*1024*1024 {
		return nil, errors.New("cannot read executor artifact within 64 MiB limit")
	}
	file, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("executor must be a Linux ELF binary: %w", err)
	}
	defer file.Close()
	if (arch == "amd64" && file.Machine != elf.EM_X86_64) || (arch == "arm64" && file.Machine != elf.EM_AARCH64) || (arch != "amd64" && arch != "arm64") {
		return nil, errors.New("executor architecture does not match inspected host")
	}
	if err := buildmeta.Check(stamp, data); err != nil {
		return nil, err
	}
	return data, nil
}

func RunLifecycle(ctx context.Context, target string, request executor.Request, binary []byte) (*lifecycle.State, error) {
	return runLifecycle(ctx, target, request, binary, process.Run)
}

func runLifecycle(ctx context.Context, target string, request executor.Request, binary []byte, run runProcess) (*lifecycle.State, error) {
	result, runErr := runStagedRequest(ctx, target, request, binary, run)
	var response executor.Response
	parseErr := decodeJSON(result.Stdout, &response)
	if parseErr == nil && response.Version == executor.Version && response.Error != nil {
		return nil, fmt.Errorf("remote %s failed: %q; inspect server status and retry the same operation after resolving the cause", request.Operation, response.Error.Message)
	}
	if runErr != nil {
		return nil, fmt.Errorf("operation outcome may be unknown; query server status before retrying: %w", connectionError(target, result, runErr))
	}
	if parseErr != nil {
		return nil, fmt.Errorf("invalid lifecycle response; query server status before retrying (decode error: %w)", parseErr)
	}
	if response.Version != executor.Version || response.HostState == nil {
		return nil, errors.New("invalid lifecycle response; query server status before retrying")
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
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("expected exactly one JSON response")
	}
	return nil
}

// runStagedRequest opens a new, non-multiplexed SSH connection for each call.
func runStagedRequest(ctx context.Context, target string, request executor.Request, binary []byte, run runProcess) (process.Result, error) {
	if err := ValidateTarget(target); err != nil {
		return process.Result{}, err
	}
	payload, err := archiveRequest(request, binary)
	if err != nil {
		return process.Result{}, err
	}
	// This command is assembled only from an embedded constant, never request fields.
	command := "/bin/sh -c " + shellQuote(bootstrapScript)
	return run(ctx, "ssh", arguments(target, adminCommand(command)), payload)
}
