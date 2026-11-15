package cli

import "github.com/nchapman/vamos/internal/deploy"

func (p *presentation) appFailure(r appReport, request deploy.Request, response deploy.Response, err error, bindingCommand string) string {
	text := p.header(r.action, r.app, r.server.Name, r.server.Target)
	unknown := r.mutation && !(response.Version == 1 && response.Error != "")
	if unknown {
		text += "\n" + p.ink("warning", "Outcome unknown", true) + "\n"
		text += p.prose("No valid acknowledgement arrived. The remote operation may still be running; disconnecting does not prove it stopped.")
		text += p.field("Diagnostic", err.Error())
		text += p.section("Inspect before retrying")
	} else {
		text += "\n" + p.ink("error", "Request failed", true) + "\n" + p.prose(err.Error())
		if r.mutation {
			text += p.section("Inspect")
		}
	}
	if r.mutation {
		args := []string{}
		if r.action == "run" {
			args = append(args, "--run", request.Operation)
		}
		text += p.command(r.command("status", args...))
		if r.action != "run" && request.Operation != "" {
			text += p.section("If recorded and resumable") + p.command(r.command("resume", request.Operation))
		}
	}
	if !unknown && response.Status != nil {
		text += p.releaseReport(response.Status.Release, r)
	}
	if bindingCommand != "" {
		text += p.section("Explicit adoption") + p.prose("Inspect status first; explicitly adopt only the intended service.") + p.command(bindingCommand)
	}
	return text
}
