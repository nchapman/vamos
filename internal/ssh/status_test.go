package ssh

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStatusScriptServiceState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remote POSIX shell behavior is exercised on Unix hosts")
	}
	// Exercise the embedded shell, replacing only its host-state path and the
	// systemctl boundary. Never query the local system manager or use SSH.
	// Empty stdout with exit 1 models a failed system bus query; the matrix
	// also covers errors accompanied by misleading recognized state output.
	for _, state := range []string{"active", "reloading", "activating", "deactivating", "inactive", "failed", "unknown", "", "unexpected", "active\ninactive"} {
		for _, code := range []int{0, 1, 3, 4, 127} {
			t.Run(fmt.Sprintf("%q/exit%d", state, code), func(t *testing.T) {
				wantActive, wantOK := false, false
				switch state {
				case "active", "reloading":
					wantActive, wantOK = true, code == 0
				case "activating", "deactivating":
					wantActive, wantOK = true, code == 0 || code == 3
				case "inactive":
					// Ubuntu 24.04 / systemd 255 reports inactive with exit 4
					// after the transient unit has been garbage-collected.
					wantOK = code == 3 || code == 4
				case "failed":
					wantOK = code == 3
				case "unknown": // systemctl's missing-unit result, not an unknown query outcome.
					wantOK = code == 3 || code == 4
				}

				dir := t.TempDir()
				fake := `#!/bin/sh
set -eu
[ "$1" = is-active ] || exit 99
shift
quiet=false
if [ "${1-}" = --quiet ]; then quiet=true; shift; fi
[ "$#" = 1 ] && [ "$1" = soar-host-operation.service ] || exit 99
printf '%s\n' 'Failed to connect to bus: private systemctl diagnostic' >&2
if [ "$quiet" = false ]; then printf '%s\n' "$FAKE_STATE"; fi
exit "$FAKE_CODE"
`
				if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(fake), 0700); err != nil {
					t.Fatal(err)
				}
				stateDir := filepath.Join(dir, "host-state")
				script := strings.ReplaceAll(statusScript, "/var/lib/soar", stateDir)
				cmd := exec.Command("/bin/sh", "-s")
				cmd.Stdin = strings.NewReader(script)
				cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FAKE_STATE="+state, fmt.Sprintf("FAKE_CODE=%d", code))
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err := cmd.Run()
				if !wantOK {
					if err == nil || stdout.Len() != 0 || stderr.String() != "cannot establish host operation service state\n" {
						t.Fatalf("unknown/contradictory state must fail safely: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
					}
					return
				}
				if err != nil || stderr.Len() != 0 {
					t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
				}
				var status Status
				if err := decodeJSON(stdout.Bytes(), &status); err != nil {
					t.Fatal(err)
				}
				if status.Active != wantActive || status.State != nil {
					t.Fatalf("status=%+v; want active=%v and no stored state", status, wantActive)
				}
			})
		}
	}
}
