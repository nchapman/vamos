package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

type helpUnreadInput struct{ t *testing.T }

func (r helpUnreadInput) Read([]byte) (int, error) { r.t.Fatal("help read stdin"); return 0, io.EOF }

func TestGroupHelpDX(t *testing.T) {
	for _, group := range []string{"service", "secrets", "apps"} {
		for _, option := range []string{"--help", "-h"} {
			t.Run(group+option, func(t *testing.T) {
				var out, diag bytes.Buffer
				var code int
				switch group {
				case "service":
					code = serviceCommand(context.Background(), []string{option}, &out, &diag, appDeps{})
				case "secrets":
					code = secretsCommand(context.Background(), []string{option}, helpUnreadInput{t}, &out, &diag, appDeps{}, nil)
				case "apps":
					code = appsCommand(context.Background(), []string{option}, &out, &diag, appDeps{})
				}
				if code != 0 || diag.Len() != 0 || !strings.Contains(out.String(), "Usage:") {
					t.Fatalf("code=%d stdout=%q stderr=%q", code, &out, &diag)
				}
				if group == "apps" {
					for _, text := range []string{"registered default", "project binding", "recorded inventory", "live health"} {
						if !strings.Contains(out.String(), text) {
							t.Errorf("help missing %q: %s", text, &out)
						}
					}
				}
			})
		}
	}
}

func TestServerOperationHelpShowsApplicableDefaults(t *testing.T) {
	for _, tc := range []struct{ action, want, absent string }{
		{"inspect", "-timeout", "-purge"},
		{"add", "-domain", "-purge"},
		{"status", "-json", "-yes"},
		{"teardown", "-purge", "-domain"},
	} {
		var out, diag bytes.Buffer
		if code := RunContext(context.Background(), []string{"server", tc.action, "--help"}, helpUnreadInput{t}, &out, &diag, "test"); code != 0 || !strings.Contains(out.String(), tc.want) || !strings.Contains(out.String(), "-timeout") || strings.Contains(out.String(), tc.absent) {
			t.Fatalf("%s: code=%d stdout=%s stderr=%s", tc.action, code, &out, &diag)
		}
	}
}

func TestSecretsSubcommandHelpDX(t *testing.T) {
	for _, action := range []string{"stage", "set", "list"} {
		for _, option := range []string{"-h", "--help"} {
			var out, diag bytes.Buffer
			code := secretsCommand(context.Background(), []string{action, option}, helpUnreadInput{t}, &out, &diag, appDeps{}, nil)
			if code != 0 || !strings.Contains(out.String(), "new values") || !strings.Contains(out.String(), "unchanged") || !strings.Contains(out.String(), "replace deployed consumers") || !strings.Contains(out.String(), "not atomic") || diag.Len() != 0 {
				t.Fatalf("%s %s: %d %q %q", action, option, code, &out, &diag)
			}
		}
	}
}
