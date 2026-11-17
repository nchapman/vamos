package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the generated POSIX shell, mocking only identity and sudo so these
// tests also run on non-root macOS without changing the test runner's privileges.
func TestAdminCommandShell(t *testing.T) {
	for _, mode := range []string{"root", "sudo", "missing", "denied"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write("id", `if [ "$MODE" = root ] || [ "$ELEVATED" = yes ]; then echo 0; else echo 1000; fi`)
			if mode != "missing" {
				write("sudo", `test "$MODE" != root || exit 99
 test "$1" = -n && test "$2" = -- || exit 98
 shift 2
 if [ "$MODE" = denied ]; then echo 'sensitive sudo diagnostic' >&2; exit 1; fi
 ELEVATED=yes; export ELEVATED
 unset SSH_CONNECTION
 exec "$@"`)
			}
			connection := "192.0.2.1 1234 192.0.2.2 22 ' ; $(exit 88)"
			command := `printf '%s\n' "$(id -u)" "$SSH_CONNECTION"; IFS= read -r input; printf '%s' "$input"`
			cmd := exec.Command("/bin/sh", "-c", adminCommand(command))
			cmd.Env = append(os.Environ(), "PATH="+dir, "MODE="+mode, "SSH_CONNECTION="+connection)
			cmd.Stdin = strings.NewReader("private payload\n")
			out, err := cmd.CombinedOutput()
			if mode == "root" || mode == "sudo" {
				if err != nil || string(out) != "0\n"+connection+"\nprivate payload" {
					t.Fatalf("out=%q err=%v", out, err)
				}
			} else {
				if err == nil || !strings.Contains(string(out), "passwordless sudo") || strings.Contains(string(out), "private") || strings.Contains(string(out), "sensitive") {
					t.Fatalf("out=%q err=%v", out, err)
				}
			}
		})
	}
}
