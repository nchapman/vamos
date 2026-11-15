package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/cli"
)

func TestInformationalCommands(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}, {"version"}, {"--version"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := cli.Run(args, &stdout, &stderr, "test-version")
			want := "Usage:"
			if len(args) > 0 && strings.Contains(args[0], "version") {
				want = "vamos test-version"
			}
			if code != 0 || !strings.Contains(stdout.String(), want) || stderr.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"unknown-command"}, {"version", "extra"}, {"help", "extra"}, {"validate", "file.yaml"}, {"validate", "--unknown"}, {"validate", "-f"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := cli.Run(args, &stdout, &stderr, "dev"); code != 2 || stderr.Len() == 0 || stdout.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
		})
	}
}

func TestValidateHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cli.Run([]string{"validate", "--help"}, &stdout, &stderr, "dev"); code != 0 || !strings.Contains(stdout.String(), "Usage:") || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
}

func TestValidateFile(t *testing.T) {
	filename := writeConfig(t, "app: notes\nservices:\n  web:\n    image: app:v1\n")
	var stdout, stderr bytes.Buffer
	code := cli.Run([]string{"validate", "-f", filename}, &stdout, &stderr, "dev")
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, &stderr)
	}
	for _, want := range []string{"valid configuration for notes", "1 services", "Local validation only"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout=%q, missing %q", &stdout, want)
		}
	}
}

func TestValidateDefaultFile(t *testing.T) {
	filename := writeConfig(t, "app: notes\nservices:\n  web: {image: app:v1}\n")
	t.Chdir(filepath.Dir(filename))
	var stdout, stderr bytes.Buffer
	if code := cli.Run([]string{"validate"}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, &stderr)
	}
}

func TestValidateFailures(t *testing.T) {
	tests := []struct{ name, filename, want string }{
		{"missing", filepath.Join(t.TempDir(), "absent.yaml"), "open config"},
		{"invalid", writeConfig(t, "app: notes\nservices: {}\n"), "at least one service"},
		{"malformed", writeConfig(t, "["), "decode YAML"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := cli.Run([]string{"validate", "-f", tt.filename}, &stdout, &stderr, "dev")
			if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
		})
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "vamos.yaml")
	if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}
