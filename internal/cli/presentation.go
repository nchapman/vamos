package cli

import (
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

// Presentation belongs to an output destination, never to stdin or a global
// renderer. Raw workload/protocol streams must not pass through these helpers.
type presentation struct {
	width   int
	theme   string
	color   bool
	profile colorprofile.Profile
}

func presentationFor(w io.Writer) *presentation {
	// A destination may explicitly provide capabilities, for example when
	// capturing a terminal transcript without owning a terminal FD.
	if provider, ok := w.(interface{ terminalPresentation() *presentation }); ok {
		return provider.terminalPresentation()
	}
	file, ok := w.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return nil
	}
	width, _, err := term.GetSize(int(file.Fd()))
	if err != nil || width <= 0 {
		width = 80
	}
	return terminalPresentationOptions(width, colorprofile.Detect(w, os.Environ()), os.Getenv)
}

func terminalPresentationOptions(width int, profile colorprofile.Profile, getenv func(string) string) *presentation {
	if getenv("TERM") == "dumb" || getenv("CI") != "" {
		return nil
	}
	if width <= 0 {
		width = 80
	}
	theme := getenv("VAMOS_THEME")
	if theme != "light" && theme != "dark" {
		theme = "terminal"
	}
	return &presentation{width: width, theme: theme, color: getenv("NO_COLOR") == "" && getenv("CLICOLOR") != "0", profile: profile}
}

// Control sequences in structured metadata are displayed literally, not
// interpreted as cursor movement, links, or bidirectional formatting.
func displayText(text string) string {
	var b strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			quoted := strconv.QuoteRune(r)
			b.WriteString(quoted[1 : len(quoted)-1])
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func (p *presentation) ink(role, text string, bold bool) string {
	text = displayText(text)
	if !p.color {
		return text
	}
	style := lipgloss.NewStyle().Bold(bold)
	// Unknown backgrounds inherit the terminal foreground for reliable contrast.
	// Explicit light/dark palettes need no terminal input/background queries.
	colors := map[string]string{}
	if p.theme == "dark" {
		colors = map[string]string{"accent": "#91b9ed", "secondary": "#a0a7b4", "warning": "#e7be6d", "error": "#f2a1a1", "success": "#98c4a5"}
	}
	if p.theme == "light" {
		colors = map[string]string{"accent": "#285a91", "secondary": "#525e6c", "warning": "#875300", "error": "#9f3030", "success": "#2f6844"}
	}
	if c := colors[role]; c != "" {
		style = style.Foreground(lipgloss.Color(c))
	}
	return style.Render(text)
}
func (p *presentation) header(action, app, server, target string) string {
	text := p.ink("accent", strings.ToUpper(action), true)
	if app != "" {
		text += "  " + p.ink("", app, true)
	}
	text += "\n"
	if server != "" {
		text += p.ink("", server, false) + "  " + p.ink("secondary", "/  "+target, false) + "\n"
	}
	return text
}
func (p *presentation) section(text string) string { return "\n" + p.ink("", text, true) + "\n" }
func (p *presentation) prose(text string) string {
	width := max(20, min(p.width, 100))
	return strings.TrimRight(lipgloss.NewStyle().Width(width).Render(displayText(text)), " ") + "\n"
}
func (p *presentation) field(label, value string) string {
	label = displayText(label)
	value = displayText(value)
	if value == "" {
		value = "(none)"
	}
	labelWidth := 22
	if p.width < 68 {
		labelWidth = max(12, ansi.StringWidth(label)+2)
	}
	if p.width < 68 && labelWidth+ansi.StringWidth(value) > p.width {
		return p.ink("secondary", label+":", false) + "\n  " + value + "\n"
	}
	return p.ink("secondary", label+strings.Repeat(" ", max(1, labelWidth-ansi.StringWidth(label))), false) + value + "\n"
}
func (p *presentation) command(command string) string {
	if displayText(command) != command {
		return p.prose("Suggested command omitted: an argument contains control characters.")
	}
	// Terminal soft wrapping preserves selection; inserted line breaks do not.
	return command + "\n"
}
func (p *presentation) write(w io.Writer, text string) error {
	var rendered strings.Builder
	profile := p.profile
	if !p.color {
		profile = colorprofile.NoTTY
	}
	cw := colorprofile.Writer{Forward: &rendered, Profile: profile}
	if _, err := io.WriteString(&cw, text); err != nil {
		return err
	}
	n, err := io.WriteString(w, rendered.String())
	if err == nil && n != rendered.Len() {
		return io.ErrShortWrite
	}
	return err
}
func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
