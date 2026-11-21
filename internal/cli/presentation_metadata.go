package cli

import (
	"fmt"
	"io"
	"strconv"

	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/secrets"
	"github.com/nchapman/vamos/internal/ssh"
)

func (p *presentation) renderServerInspect(target string, report *host.Report) string {
	text := p.header("server inspect", "", "", "") + p.field("Target", target)
	text += p.prose("Read-only inspection; no setup changes made.")
	text += p.field("Compatible host", yesNo(report.Supported))
	if report.Root {
		text += p.field("Privilege", "root (no setup changes made)")
	} else {
		text += p.field("Privilege", "non-root")
		text += p.prose("Privileged setup capability was not checked (sudo was not invoked).")
	}
	if len(report.Problems) > 0 {
		text += p.section("Problems")
		for _, problem := range report.Problems {
			text += p.prose(problem)
		}
	}
	text += p.section("Observed host")
	text += p.field("OS", report.OS+" "+report.Version)
	text += p.field("Kernel", report.Kernel)
	text += p.field("Architecture", report.Architecture)
	text += p.field("Systemd", yesNo(report.Systemd))
	text += p.field("UID", strconv.FormatInt(int64(report.UID), 10))
	return text
}

func (p *presentation) renderServerStatus(name, target string, status *ssh.Status) string {
	text := p.header("server status", "", name, target)
	text += p.prose("Observed host lifecycle state; not an app health check.")
	phase, step := "unmanaged", ""
	if status.State != nil {
		phase, step = status.State.Phase, status.State.Step
	}
	text += p.field("Phase", phase) + p.field("Step", step)
	text += p.field("Active operation", yesNo(status.Active))
	if status.State != nil && status.State.LastError != "" {
		text += p.section("Last error") + p.prose(status.State.LastError)
	}
	return text
}

func writeServerMutationStart(w io.Writer, operation, name, target, domain string) error {
	var notice string
	switch operation {
	case "add":
		notice = caddyPatchNotice
	case "upgrade":
		notice = serverUpgradeImpact
	}
	if p := presentationFor(w); p != nil {
		text := p.header("server "+operation, "", name, target)
		text += p.ink("warning", "Root host mutation authorized", true) + "\n"
		text += p.field("Domain", domain)
		if notice != "" {
			text += p.prose(notice)
		}
		return p.write(w, text)
	}
	_, err := fmt.Fprintf(w, "%s / %s / %s: %s (root host mutation authorized)\n", name, target, domain, operation)
	if err == nil && notice != "" {
		_, err = fmt.Fprintln(w, notice)
	}
	return err
}

func writeServerMutationResult(w io.Writer, name string, state *lifecycle.State, message string) error {
	if p := presentationFor(w); p != nil {
		text := p.section("Host operation result") + p.field("Server", name)
		text += p.field("Phase", state.Phase) + p.field("Installation", state.ID)
		return p.write(w, text+p.prose(message))
	}
	if _, err := fmt.Fprintf(w, "%s: %s (installation %s)\n", name, state.Phase, state.ID); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, message)
	return err
}

func writeSecretStart(w io.Writer, action, server, target, app, name, operation string) error {
	if p := presentationFor(w); p != nil {
		text := p.header("secrets "+action, app, server, target)
		text += p.field("Secret", name) + p.field("Operation/version", operation)
		return p.write(w, text)
	}
	_, err := fmt.Fprintf(w, "%s / %s: %s %s (operation/version %s)\n", server, app, action, name, operation)
	return err
}

// message is a static, action-appropriate refusal, never a remote diagnostic.
// These renderers accept only selected metadata, not secret input or responses.
func writeSecretFailure(w io.Writer, action, server, target, app, operation, message string) {
	if p := presentationFor(w); p != nil {
		text := p.header("secrets "+action, app, server, target)
		if message != "" {
			text += p.ink("warning", "Request refused", true) + "\n" + p.prose(message)
		}
		if action == "stage" {
			outcome := "Secret staging outcome unknown"
			if message != "" {
				outcome = "Staging refused"
			}
			text += p.ink("warning", outcome, true) + "\n"
			text += p.field("Operation/version", operation)
			text += p.section("Inspect") + p.command(suggestedCommand("vamos", "secrets", "list", "--server", server, "--app", app))
		}
		_ = p.write(w, text)
		return
	}
	if message != "" {
		fmt.Fprintln(w, message)
	}
	if action == "stage" {
		outcome := "Secret staging outcome unknown"
		if message != "" {
			outcome = "Staging refused"
		}
		fmt.Fprintf(w, "%s (operation/version %s).\nInspect: vamos secrets list --server %s --app %s\n", outcome, operation, server, app)
	}
}

func (p *presentation) renderSecretList(server, target, app string, entries []secrets.Entry) string {
	text := p.header("secrets list", app, server, target)
	text += p.prose("Retained secret metadata only; values are not displayed.")
	if len(entries) == 0 {
		return text + p.prose("No retained secret versions reported.")
	}
	for _, entry := range entries {
		text += p.section(entry.Name) + p.field("Version", entry.Version)
		text += p.field("Latest", yesNo(entry.Latest))
	}
	return text
}

func writeInitSuccess(w io.Writer, app, filename, bindingMessage string) error {
	if p := presentationFor(w); p != nil {
		text := p.header("init", app, "", "")
		text += p.ink("success", "Local configuration created", true) + "\n"
		text += p.field("File", filename) + p.field("Binding", bindingMessage)
		text += p.prose("Uses Dockerfile with context '.'; no Dockerfile created and no remote contacted.")
		return p.write(w, text)
	}
	_, err := fmt.Fprintf(w, "Created %s; %s. Uses Dockerfile with context '.'; no Dockerfile created and no remote contacted.\n", filename, bindingMessage)
	return err
}
