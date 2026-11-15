package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

func TestPresentationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name           string
		env            map[string]string
		enabled, color bool
		theme          string
	}{
		{"default", nil, true, true, "terminal"},
		{"light", map[string]string{"VAMOS_THEME": "light"}, true, true, "light"},
		{"dark", map[string]string{"VAMOS_THEME": "dark"}, true, true, "dark"},
		{"invalid-theme", map[string]string{"VAMOS_THEME": "unknown"}, true, true, "terminal"},
		{"no-color", map[string]string{"NO_COLOR": "1"}, true, false, "terminal"},
		{"clicolor", map[string]string{"CLICOLOR": "0"}, true, false, "terminal"},
		{"ci", map[string]string{"CI": "true"}, false, false, ""},
		{"dumb", map[string]string{"TERM": "dumb"}, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := terminalPresentationOptions(0, colorprofile.TrueColor, func(k string) string { return tc.env[k] })
			if (p != nil) != tc.enabled {
				t.Fatalf("enabled=%v", p != nil)
			}
			if p != nil && (p.color != tc.color || p.theme != tc.theme || p.width != 80) {
				t.Fatalf("options=%+v", p)
			}
		})
	}
}
func TestPresentationDoesNotForceStylesIntoPipes(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("FORCE_COLOR", "1")
	t.Setenv("VAMOS_THEME", "dark")
	var b bytes.Buffer
	if presentationFor(&b) != nil {
		t.Fatal("ordinary writer treated as a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if presentationFor(w) != nil {
		t.Fatal("pipe treated as a terminal")
	}
}
func TestPresentationEscapesControlsButPreservesCommandArguments(t *testing.T) {
	p := newHelpAppsTerminal(48, true).p
	unsafe := "hello\x1b[2J\r\n\t\u202eEND"
	rendered := p.field("Field", unsafe)
	plain := ansi.Strip(rendered)
	if !strings.Contains(plain, `hello\x1b[2J\r\n\t\u202eEND`) {
		t.Fatal(plain)
	}
	command := suggestedCommand("vamos", "deploy", "-f", "a user's long configuration.yaml", "--server", "staging")
	if got := p.command(command); got != command+"\n" {
		t.Fatalf("command changed: %q", got)
	}
	if got := p.command("vamos deploy -f 'unsafe\npath'"); !strings.Contains(got, "omitted") || strings.Contains(got, "vamos deploy") {
		t.Fatal(got)
	}
}
func TestPresentationColorProfilesAndWriteErrors(t *testing.T) {
	for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI256, colorprofile.ANSI} {
		p := newHelpAppsTerminal(80, true).p
		p.profile = profile
		var out bytes.Buffer
		if err := p.write(&out, p.ink("accent", "message", true)); err != nil {
			t.Fatal(err)
		}
		if ansi.Strip(out.String()) != "message" {
			t.Fatal(out.String())
		}
		if profile != colorprofile.TrueColor && strings.Contains(out.String(), "38;2;") {
			t.Fatal("true color not downsampled")
		}
	}
	p := newHelpAppsTerminal(80, false).p
	var out bytes.Buffer
	if err := p.write(&out, "\x1b[1mtext\x1b[0m"); err != nil || out.String() != "text" {
		t.Fatalf("mono=%q err=%v", out.String(), err)
	}
	if err := p.write(shortPresentationWriter{}, "content"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err=%v", err)
	}
	sentinel := errors.New("write failed")
	if err := p.write(failingPresentationWriter{sentinel}, "content"); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
}

type shortPresentationWriter struct{}

func (shortPresentationWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }

type failingPresentationWriter struct{ err error }

func (w failingPresentationWriter) Write([]byte) (int, error) { return 0, w.err }
