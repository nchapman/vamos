package cli

import (
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// writeHelpText is for successful help only. Parse diagnostics must retain their
// original stderr path and must not be reinterpreted as successful help.
func writeHelpText(w io.Writer, text string) error {
	if p := presentationFor(w); p != nil {
		return p.write(w, p.renderHelp(text))
	}
	_, err := io.WriteString(w, text)
	return err
}

// Keep the complete captured help, including indentation, blank lines, defaults,
// and examples. Styling is deliberately line-based, not a second help parser.
func (p *presentation) renderHelp(text string) string {
	var rendered strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		body := strings.TrimSuffix(line, "\n")
		trimmed := strings.TrimSpace(body)
		role, bold := "", false
		switch {
		case strings.HasPrefix(trimmed, "Usage:"), strings.HasSuffix(trimmed, ":"):
			bold = true
		case strings.HasPrefix(trimmed, "vamos "):
			role = "accent"
		}
		// Tabs belong to flag.PrintDefaults' layout, not to a structured value.
		parts := strings.Split(body, "\t")
		for i, part := range parts {
			if i > 0 {
				rendered.WriteByte('\t')
			}
			rendered.WriteString(p.ink(role, part, bold))
		}
		if strings.HasSuffix(line, "\n") {
			rendered.WriteByte('\n')
		}
	}
	return rendered.String()
}

func writeMainHelp(w io.Writer) error {
	if p := presentationFor(w); p != nil {
		return p.write(w, p.renderMainHelp())
	}
	_, err := io.WriteString(w, help)
	return err
}

func (p *presentation) renderMainHelp() string {
	type command struct{ name, description string }
	groups := []struct {
		title    string
		commands []command
	}{
		{"Work with an app", []command{
			{"init", "Write a minimal app configuration"},
			{"validate", "Validate local app configuration"},
			{"deploy", "Build and deploy services"},
			{"exec", "Run a command inside an existing service"},
			{"console", "Open a shell in an existing service"},
			{"run", "Run a noninteractive command"},
			{"service bind|remove", "Bind locally or preview/remove a service"},
			{"secrets stage|set|list", "Stage only, set new values and replace consumers, or list versions"},
		}},
		{"Inspect and recover", []command{
			{"apps", "List recorded inventory, not live health"},
			{"status", "Inspect app operation and runtime state"},
			{"logs", "Read or follow container logs"},
			{"rollback", "Preview or restore captured service images"},
			{"resume", "Resume captured operation inputs"},
			{"cleanup", "Preview or remove obsolete app resources"},
			{"destroy", "Preview or remove an app"},
		}},
		{"Manage backups", []command{
			{"backup repository", "Register and inspect backup repositories"},
			{"backup configure", "Preview or apply backup configuration"},
			{"backup plan", "Inspect recorded backup plan metadata"},
		}},
		{"Manage a server", []command{
			{"server inspect", "Inspect a host over SSH without provisioning"},
			{"server add", "Bootstrap a dedicated Ubuntu host"},
			{"server upgrade", "Update host components; may interrupt public traffic"},
			{"server connect", "Register an existing ready host without remote changes"},
			{"server principal", "Grant, list, and revoke single-app deploy keys"},
			{"server firewall", "Manage opt-in dedicated-host firewall protection"},
			{"server status", "Read the host lifecycle journal"},
			{"server teardown", "Remove an unused owned installation"},
		}},
		{"Reference", []command{
			{"upgrade", "Upgrade the CLI and Linux executor binaries"},
			{"version", "Print the build version"},
			{"help", "Show general or command-specific help"},
		}},
	}
	text := p.header("vamos", "", "", "")
	text += p.prose("A small app platform for one server")
	text += p.section("Usage") + p.command("vamos COMMAND [OPTIONS]")
	for _, group := range groups {
		text += p.section(group.title)
		commandWidth, descriptionWidth := 0, 0
		for _, cmd := range group.commands {
			commandWidth = max(commandWidth, ansi.StringWidth("vamos "+cmd.name))
			descriptionWidth = max(descriptionWidth, ansi.StringWidth(cmd.description))
		}
		for _, cmd := range group.commands {
			name := "vamos " + cmd.name
			if commandWidth+2+descriptionWidth <= p.width {
				text += p.ink("accent", name, false) + strings.Repeat(" ", commandWidth-ansi.StringWidth(name)+2)
				text += p.ink("", cmd.description, false) + "\n"
			} else {
				text += p.ink("accent", name, false) + "\n" + p.prose(cmd.description)
			}
		}
	}
	text += p.section("Examples")
	text += p.command("vamos apps --server home")
	text += p.command("vamos status --server home --app notes")
	text += p.command("vamos help deploy")
	// Keep the existing safety and scope qualifications alongside the new
	// navigation; this is still help, not a deployment success claim.
	text += p.section("Before you begin")
	text += p.prose("Setup/teardown require explicit authorization.") + "\n"
	text += p.prose("Server administration requires root or passwordless sudo; server inspect remains unelevated.") + "\n"
	text += p.prose("Deploy supports built or prebuilt public HTTP, private, and direct-port services, with one optional named volume per service.") + "\n"
	text += p.prose("Use deploy --service NAME for scoped updates.")
	text += p.prose("Existing manual services require --yes.")
	text += p.prose("Data-backed updates stop/start; data is retained.")
	text += p.prose("Run a command with --help for options.")
	return text
}
