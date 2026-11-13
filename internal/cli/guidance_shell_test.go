//go:build !windows

package cli

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSuggestedCommandPreservesShellArgumentBoundaries(t *testing.T) {
	args := []string{"", "path with spaces/user's.yaml", "$(printf INJECTED)", "; printf INJECTED", "line\nbreak", "@value", "--flag", "normal"}
	output, err := exec.Command("/bin/sh", "-c", "printf '%s\\000' "+suggestedCommand(args...)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != strings.Join(args, "\x00")+"\x00" {
		t.Fatalf("argument boundaries changed: %q", output)
	}
}
