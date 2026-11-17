package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/nchapman/vamos/internal/firewall"
)

func (p *presentation) firewallStatus(name string, s *firewall.Status) string {
	text := p.header("server firewall", "", name, "")
	label, tone := "Protection disabled", "secondary"
	if s.Enabled {
		label, tone = "Protection enabled", "success"
	}
	if s.Pending {
		label, tone = "Confirmation pending", "warning"
	}
	if s.Degraded {
		label, tone = "Needs attention", "warning"
	}
	text += "\n" + p.ink(tone, label, true) + "\n"
	text += p.field("Enabled", yesNo(s.Enabled)) + p.field("Pending confirmation", yesNo(s.Pending)) + p.field("Degraded", yesNo(s.Degraded))
	ports := make([]string, len(s.SSHPorts))
	for i, port := range s.SSHPorts {
		ports[i] = strconv.Itoa(port)
	}
	if len(ports) > 0 {
		text += p.field("SSH allowances (TCP)", strings.Join(ports, ", "))
	}
	if s.Pending {
		text += p.prose("Unconfirmed changes have automatic rollback armed. Enable confirms through a fresh SSH connection; inspect status before retrying.")
	}
	if !s.Enabled && !s.Degraded && !s.Pending {
		text += p.prose("Vamos-managed protection is disabled. This does not describe provider or other host firewalls; published ports remain configured.")
	}
	if s.Enabled && !s.Pending && !s.Degraded {
		text += p.prose("Allows SSH, HTTP/HTTPS, and intentional Docker TCP/UDP publishing. Application health and external reachability are not checked.")
	}
	if len(s.Problems) > 0 {
		text += p.section("Problems")
		for _, problem := range s.Problems {
			text += p.prose(problem)
		}
	}
	if s.Degraded {
		text += p.prose("Resolve ownership or drift errors without flushing rules or adopting foreign configuration.")
	}
	if s.Pending || s.Degraded {
		text += p.section("Inspect before retrying") + p.command(suggestedCommand("vamos", "server", "firewall", "status", name))
	}
	return text
}

func firewallFailure(out io.Writer, operation, name, target, artifact string, err error) int {
	args := []string{"vamos", "server", "firewall", "status"}
	if artifact != "" {
		args = append(args, "--executor", artifact)
	}
	command := suggestedCommand(append(args, name)...)
	guidance := "Inspect status before retrying; the response alone does not establish the current firewall state."
	if operation == "disable" {
		guidance = "Disable has no automatic rollback. Inspect status before retrying; resolve any ownership or drift errors rather than flushing rules."
	}
	if p := presentationFor(out); p != nil {
		text := p.header("server firewall "+operation, "", name, target)
		text += "\n" + p.ink("warning", "Firewall operation not confirmed", true) + "\n" + p.prose(err.Error())
		text += p.prose(guidance) + p.section("Inspect before retrying") + p.command(command)
		_ = p.write(out, text)
	} else {
		fmt.Fprintf(out, "error: %v\n%s\nInspect: %s\n", err, guidance, command)
	}
	return 1
}
