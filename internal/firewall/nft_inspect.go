package firewall

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// Ubuntu docker.io installs libnftables1 without necessarily installing the nft
// executable. Use the same public inspection API through Python's standard
// library when the CLI is absent. Never infer an empty ruleset from missing tools.
func (m *Manager) nft(ctx context.Context, json bool) (string, error) {
	args := []string{"list", "tables"}
	command := "list tables"
	if json {
		args = []string{"-j", "list", "ruleset"}
		command = "list ruleset"
	}
	out, err := m.command(ctx, "nft", args...)
	var exit interface{ ExitCode() int }
	missing := errors.Is(err, exec.ErrNotFound) || (errors.As(err, &exit) && exit.ExitCode() == 127)
	if err == nil || !missing {
		return out, err
	}
	flag := "0"
	if json {
		flag = "16"
	} // NFT_CTX_OUTPUT_JSON, libnftables.h public API.
	out, err = m.command(ctx, "python3", "-c", nftLibraryInspect, flag, command)
	if err != nil {
		return "", fmt.Errorf("native nft inspection requires nft CLI or python3 + libnftables.so.1: %w", err)
	}
	return out, nil
}

const nftLibraryInspect = `import ctypes, sys
n = ctypes.CDLL("libnftables.so.1")
n.nft_ctx_new.argtypes = [ctypes.c_uint]
n.nft_ctx_new.restype = ctypes.c_void_p
n.nft_ctx_free.argtypes = [ctypes.c_void_p]
n.nft_ctx_output_set_flags.argtypes = [ctypes.c_void_p, ctypes.c_uint]
for name in ("nft_ctx_buffer_output", "nft_ctx_buffer_error"):
    getattr(n, name).argtypes = [ctypes.c_void_p]
    getattr(n, name).restype = ctypes.c_int
for name in ("nft_ctx_get_output_buffer", "nft_ctx_get_error_buffer"):
    getattr(n, name).argtypes = [ctypes.c_void_p]
    getattr(n, name).restype = ctypes.c_char_p
n.nft_run_cmd_from_buffer.argtypes = [ctypes.c_void_p, ctypes.c_char_p]
n.nft_run_cmd_from_buffer.restype = ctypes.c_int
ctx = n.nft_ctx_new(0)
if not ctx:
    raise RuntimeError("cannot allocate nft inspection context")
try:
    n.nft_ctx_output_set_flags(ctx, int(sys.argv[1]))
    if n.nft_ctx_buffer_output(ctx) != 0 or n.nft_ctx_buffer_error(ctx) != 0:
        raise RuntimeError("cannot buffer nft inspection output")
    result = n.nft_run_cmd_from_buffer(ctx, sys.argv[2].encode())
    diagnostic = n.nft_ctx_get_error_buffer(ctx) or b""
    if result != 0 or diagnostic:
        raise RuntimeError("nft inspection failed: " + diagnostic.decode(errors="replace"))
    sys.stdout.buffer.write(n.nft_ctx_get_output_buffer(ctx) or b"")
finally:
    n.nft_ctx_free(ctx)
`
