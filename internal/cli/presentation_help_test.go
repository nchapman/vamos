package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// This explicit destination avoids terminal queries and process-wide state.
// Its name is intentionally distinct from the other presentation fixtures.
type helpAppsTerminalBuffer struct {
	bytes.Buffer
	p *presentation
}

func (w *helpAppsTerminalBuffer) terminalPresentation() *presentation { return w.p }

func newHelpAppsTerminal(width int, color bool) *helpAppsTerminalBuffer {
	return &helpAppsTerminalBuffer{p: &presentation{width: width, theme: "dark", color: color, profile: colorprofile.TrueColor}}
}

func TestMainHelpPresentation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		width int
		color bool
	}{{"wide-color", 120, true}, {"narrow-mono", 48, false}} {
		t.Run(tc.name, func(t *testing.T) {
			out := newHelpAppsTerminal(tc.width, tc.color)
			var diag bytes.Buffer
			if code := Run([]string{"help"}, out, &diag, "test"); code != 0 || diag.Len() != 0 {
				t.Fatalf("code=%d stderr=%q", code, diag.String())
			}
			text := ansi.Strip(out.String())
			for _, want := range []string{"SOAR", "Work with an app", "Inspect and recover", "Manage a server", "Reference", "Examples", "Before you begin", "Setup/teardown require explicit", "data is retained.", "soar apps --server home", "soar status --server home --app notes", "soar help deploy"} {
				if !strings.Contains(strings.Join(strings.Fields(text), " "), want) {
					t.Errorf("missing %q in:\n%s", want, text)
				}
			}
			for _, command := range []string{"apps", "init", "exec", "console", "run", "deploy", "status", "logs", "rollback", "resume", "cleanup", "destroy", "service bind|remove", "secrets stage|list", "validate", "server inspect", "server add", "server status", "server teardown", "version", "help"} {
				if !strings.Contains(text, "soar "+command) {
					t.Errorf("missing command %q", command)
				}
			}
			if strings.Contains(out.String(), "\x1b[") != tc.color {
				t.Errorf("color=%v output=%q", tc.color, out.String())
			}
			for _, line := range strings.Split(text, "\n") {
				if ansi.StringWidth(line) > tc.width {
					t.Errorf("line exceeds %d columns: %q", tc.width, line)
				}
			}
		})
	}
}

func TestMainHelpPlainContract(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"-h"}, {"--help"}} {
		var out, diag bytes.Buffer
		if code := Run(args, &out, &diag, "test"); code != 0 || out.String() != help || diag.Len() != 0 {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", args, code, out.String(), diag.String())
		}
	}
}

func TestSuccessfulCommandHelpPresentationPreservesCapturedText(t *testing.T) {
	for _, args := range [][]string{{"help", "deploy"}, {"status", "--help"}, {"apps", "--help"}, {"validate", "-h"}, {"help", "server", "add"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			var plain, diag bytes.Buffer
			if code := Run(args, &plain, &diag, "test"); code != 0 || diag.Len() != 0 {
				t.Fatalf("plain code=%d stderr=%q", code, diag.String())
			}
			for _, color := range []bool{true, false} {
				out := newHelpAppsTerminal(48, color)
				if code := Run(args, out, &diag, "test"); code != 0 || diag.Len() != 0 {
					t.Fatalf("terminal code=%d stderr=%q", code, diag.String())
				}
				if got := ansi.Strip(out.String()); got != plain.String() {
					t.Fatalf("help content changed:\ngot  %q\nwant %q", got, plain.String())
				}
				if strings.Contains(out.String(), "\x1b[") != color {
					t.Fatalf("color=%v output=%q", color, out.String())
				}
			}
		})
	}
}

func TestParseFlagsPresentationOnlyForSuccessfulHelp(t *testing.T) {
	const captured = "Usage: soar fixture [OPTIONS]\n\nOptions:\n  -name string\n    \ta name (default \"sample\")\n\nExample:\n  soar fixture --name sample\n"
	for _, args := range [][]string{{"--help"}, {"--unknown"}} {
		out, diag := newHelpAppsTerminal(48, true), newHelpAppsTerminal(48, true)
		flags := flag.NewFlagSet("fixture", flag.ContinueOnError)
		flags.Usage = func() { fmt.Fprint(flags.Output(), captured) }
		err := parseFlags(flags, args, out, diag)
		if args[0] == "--help" {
			if !errors.Is(err, flag.ErrHelp) || diag.Len() != 0 || ansi.Strip(out.String()) != captured {
				t.Fatalf("err=%v stdout=%q stderr=%q", err, out.String(), diag.String())
			}
		} else {
			want := "flag provided but not defined: -unknown\n" + captured
			if err == nil || errors.Is(err, flag.ErrHelp) || out.Len() != 0 || diag.String() != want {
				t.Fatalf("err=%v stdout=%q stderr=%q", err, out.String(), diag.String())
			}
		}
	}
}

func TestHelpPresentationEscapesControlsWithoutLosingText(t *testing.T) {
	out := newHelpAppsTerminal(48, false)
	if err := writeHelpText(out, "Usage: soar test\n  unsafe\x1b[2J\rname\n"); err != nil {
		t.Fatal(err)
	}
	if want := "Usage: soar test\n  unsafe\\x1b[2J\\rname\n"; out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
}
