package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestPublicHelpDispatchAndStreams(t *testing.T) {
	for _, path := range []string{"apps", "init", "exec", "console", "run", "deploy", "status", "logs", "rollback", "resume", "cleanup", "destroy", "service", "service bind", "service remove", "secrets", "secrets stage", "secrets list", "validate", "server", "server inspect", "server add", "server status", "server teardown", "version"} {
		for _, prefix := range []bool{false, true} {
			t.Run(path+map[bool]string{true: "/help-command", false: "/flag"}[prefix], func(t *testing.T) {
				args := strings.Fields(path)
				if prefix {
					args = append([]string{"help"}, args...)
				} else {
					args = append(args, "--help")
				}
				var out, diagnostic bytes.Buffer
				code := RunContext(context.Background(), args, helpUnreadInput{t}, &out, &diagnostic, "test")
				if code != 0 || diagnostic.Len() != 0 || !strings.Contains(out.String(), "Usage:") {
					t.Fatalf("code=%d stdout=%q stderr=%q", code, &out, &diagnostic)
				}
			})
		}
	}
}

func TestHelpRejectsArgumentsAndInternalEntrypoints(t *testing.T) {
	for _, args := range [][]string{{"help", "deploy", "--yes"}, {"help", "deploy", "-f"}, {"help", "run", "echo"}, {"help", "internal-app-executor"}, {"help", "internal-session-cleanup"}, {"help", "server", "bogus"}, {"help", "server", "add", "home"}, {"help", "help"}} {
		var out, diagnostic bytes.Buffer
		if code := RunContext(context.Background(), args, helpUnreadInput{t}, &out, &diagnostic, "test"); code != 2 || out.Len() != 0 {
			t.Fatalf("%v: code=%d out=%s err=%s", args, code, &out, &diagnostic)
		}
	}
}

func TestFlagErrorsStayOffStdout(t *testing.T) {
	for _, path := range []string{"apps", "init", "exec", "console", "deploy", "secrets list", "validate", "server inspect", "server add"} {
		var out, diagnostic bytes.Buffer
		args := append(strings.Fields(path), "--not-a-real-option")
		if code := RunContext(context.Background(), args, helpUnreadInput{t}, &out, &diagnostic, "test"); code != 2 || out.Len() != 0 || diagnostic.Len() == 0 {
			t.Fatalf("%v: code=%d out=%s err=%s", args, code, &out, &diagnostic)
		}
	}
}
