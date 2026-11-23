package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/registry"
)

type appReport struct {
	action, app, service string
	server               registry.Server
	mutation, apply      bool
	extraFlags           []string
	approval             string
}

func (r appReport) command(action string, extra ...string) string {
	args := []string{"vamos", action, "--server", r.server.Name, "--app", r.app}
	for i := 0; i < len(r.extraFlags); i++ {
		flag := r.extraFlags[i]
		key := strings.SplitN(flag, "=", 2)[0]
		overridden := false
		for _, value := range extra {
			if value == key && strings.HasPrefix(key, "--") {
				overridden = true
				break
			}
		}
		if overridden {
			if !strings.Contains(flag, "=") && key != "--candidate" && i+1 < len(r.extraFlags) {
				i++
			}
			continue
		}
		args = append(args, flag)
	}
	args = append(args, extra...)
	return suggestedCommand(args...)
}
func selectedServices(s *deploy.Status, selected string) []string {
	var names []string
	for name := range s.Services {
		if selected == "" || name == selected {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
func (p *presentation) statusReport(s *deploy.Status, r appReport) string {
	var b strings.Builder
	if !r.mutation {
		b.WriteString(p.header(r.action, r.app, r.server.Name, r.server.Target))
	}
	names := selectedServices(s, r.service)
	attention := s.LastError != ""
	inspectService := ""
	for _, name := range names {
		ss := s.Services[name]
		if ss.Unavailable {
			attention = true
			if inspectService == "" {
				inspectService = name
			}
		}
	}
	for _, v := range s.Volumes {
		if (r.service == "" || r.service == v.Service) && v.Missing {
			attention = true
		}
	}
	if attention {
		b.WriteString("\n" + p.ink("warning", "Needs attention", true) + "\n")
		if s.LastError != "" {
			b.WriteString(p.field("Last error", s.LastError))
		}
		for _, name := range names {
			ss := s.Services[name]
			if ss.Unavailable {
				b.WriteString(p.prose(name + ": service is unavailable."))
			}
			if ss.DataMayHaveChanged {
				b.WriteString(p.prose(name + ": application data may have changed; rollback does not restore data."))
			}
		}
		if inspectService != "" {
			args := []string{"--service", inspectService}
			ss := s.Services[inspectService]
			instances := ss.Instances
			if ss.Candidate != "" || len(ss.CandidateInstances) > 0 {
				instances = ss.CandidateInstances
				args = append(args, "--candidate")
			}
			if len(instances) > 1 {
				args = append(args, "--replica", fmt.Sprint(instances[0].Ordinal))
			}
			b.WriteString(p.section("Inspect before retrying"))
			b.WriteString(p.command(r.command("logs", args...)))
		}
	}
	if s.Phase == "staged" {
		b.WriteString("\n" + p.prose("No deployment recorded; staged secrets are retained."))
	}
	if len(names) > 0 {
		b.WriteString(p.section("Services"))
		b.WriteString(p.serviceSummary(s, names))
	}
	if len(s.Jobs) > 0 {
		b.WriteString(p.section("Scheduled jobs"))
		for _, job := range s.Jobs {
			b.WriteString(p.field(job.Service+" / "+job.Name, job.Schedule))
			b.WriteString(p.field("Next scheduled", job.Next))
			b.WriteString(jobStatusFields(p, job))
		}
	}
	b.WriteString(p.section("Recorded operation"))
	b.WriteString(p.field("Phase", s.Phase))
	b.WriteString(p.field("Operation", s.Operation))
	b.WriteString(p.field("Host operation active", yesNo(s.Active)))
	b.WriteString(p.releaseReport(s.Release, r))
	if len(names) > 0 {
		for _, name := range names {
			ss := s.Services[name]
			b.WriteString(p.section(name + " · recorded state"))
			b.WriteString(p.field("Recorded phase", ss.Phase))
			b.WriteString(p.field("Data may have changed", yesNo(ss.DataMayHaveChanged)))
			if ss.URL != "" {
				b.WriteString(p.field("URL", ss.URL))
			}
			b.WriteString(p.field("Operation", ss.Operation))
			b.WriteString(p.field("Container", ss.Container))
			b.WriteString(p.field("Image", ss.Image))
			if ss.Candidate != "" {
				b.WriteString(p.field("Candidate", ss.Candidate))
			}
			b.WriteString(p.publishedPortReport(name, ss))
			b.WriteString(p.replicaReport(ss))
			b.WriteString(p.domainReport(ss.Domains))
		}
	} else if len(s.Services) == 0 && s.Phase != "staged" {
		b.WriteString(p.section("web"))
		b.WriteString(p.field("Running", yesNo(s.Running)))
		b.WriteString(p.field("URL", "https://"+r.app+"."+r.server.Domain))
		b.WriteString(p.field("Container", s.Container))
	}
	b.WriteString(p.retainedReport(s, r.service))
	if r.mutation {
		b.WriteString("\n")
		for _, notice := range strings.Split(deploymentRouteNotice(s), "\n") {
			b.WriteString(p.prose(notice))
		}
	}
	return b.String()
}
func (p *presentation) serviceSummary(s *deploy.Status, names []string) string {
	var b strings.Builder
	nameWidth := 7
	kindWidth := 4
	for _, name := range names {
		nameWidth = max(nameWidth, len(displayText(name)))
		kindWidth = max(kindWidth, len(displayText(s.Services[name].Kind)))
	}
	fits := nameWidth+kindWidth+39 <= p.width
	if fits {
		b.WriteString(p.ink("secondary", fmt.Sprintf("%-*s  %-*s  %-8s  %-10s  %s", nameWidth, "SERVICE", kindWidth, "KIND", "RUNNING", "AVAILABLE", "UNAVAILABLE"), false) + "\n")
	}
	for index, name := range names {
		ss := s.Services[name]
		if !fits && index > 0 {
			b.WriteByte('\n')
		}
		// Available and Unavailable are independent facts, not inverses.
		if fits {
			fmt.Fprintf(&b, "%-*s  %-*s  %-8s  %-10s  %s\n", nameWidth, displayText(name), kindWidth, displayText(ss.Kind), yesNo(ss.Running), yesNo(ss.Available), yesNo(ss.Unavailable))
		} else {
			b.WriteString(p.ink("", name, true) + "  " + p.ink("secondary", ss.Kind, false) + "\n")
			b.WriteString(p.prose("Running " + yesNo(ss.Running) + " / Available " + yesNo(ss.Available) + " / Unavailable " + yesNo(ss.Unavailable)))
		}
	}
	return b.String()
}
func (p *presentation) domainReport(domains []deploy.DomainStatus) string {
	var b strings.Builder
	for _, d := range domains {
		b.WriteString("\n" + p.field("Domain", "https://"+d.Hostname))
		b.WriteString(p.field("Route", d.Route))
		b.WriteString(p.field("DNS", d.DNS))
		b.WriteString(p.field("Certificate", d.Certificate))
		b.WriteString(p.field("HTTPS", d.HTTPS))
		if note := domainStatusNote(d); note != "" {
			b.WriteString(p.prose(note))
		}
	}
	return b.String()
}
func (p *presentation) retainedReport(s *deploy.Status, selected string) string {
	var b strings.Builder
	for _, v := range s.Volumes {
		if selected != "" && selected != v.Service {
			continue
		}
		b.WriteString(p.section("Volume · " + v.Service))
		b.WriteString(p.field("Name", v.Name))
		b.WriteString(p.field("Mount", v.Mount))
		b.WriteString(p.field("ID", v.ID))
		b.WriteString(p.field("State", v.State))
		b.WriteString(p.field("Missing", yesNo(v.Missing)))
		b.WriteString(p.field("Data may have changed", yesNo(v.DataMayHaveChanged)))
	}
	for _, s := range s.Secrets {
		b.WriteString(p.section("Secret · " + s.Name))
		b.WriteString(p.field("Staged version", s.StagedVersion))
		b.WriteString(p.field("Applied version", s.AppliedVersion))
		b.WriteString(p.field("Pending", yesNo(s.Pending)))
		b.WriteString(p.field("Services", strings.Join(s.Services, ", ")))
		names := make([]string, 0, len(s.AppliedVersions))
		for name := range s.AppliedVersions {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			b.WriteString(p.field("Applied · "+name, s.AppliedVersions[name]))
		}
	}
	return b.String()
}
func maintenanceDetailNotice(plan *deploy.Plan) string {
	if !plan.DetailsIncomplete {
		return ""
	}
	text := "Details are incomplete: displaying a bounded subset."
	if totals := plan.Totals; totals != nil {
		text += fmt.Sprintf("\nExact totals: containers=%d images=%d archives/files=%d retained=%d.", totals.Containers, totals.Images, totals.Archives, totals.Retained)
	}
	return text + "\nApproval covers the entire recomputed plan, not just the displayed details."
}

func (p *presentation) maintenanceReport(response deploy.Response, r appReport) string {
	var b strings.Builder
	if !r.mutation {
		b.WriteString(p.header(r.action, r.app, r.server.Name, r.server.Target))
	}
	message := "Maintenance completed."
	if !r.mutation {
		message = "Recorded maintenance plan."
	}
	role := ""
	if !r.apply && (r.action == "destroy" || r.action == "cleanup" || r.action == "service-remove") {
		message = "Preview only: no resources deleted."
		role = "warning"
	} else if r.action == "destroy" || (response.Status != nil && response.Status.Phase == "destroyed") {
		message = "App destroyed; no live route is configured."
	}
	b.WriteString("\n" + p.ink(role, message, true) + "\n")
	if plan := response.Plan; plan != nil {
		b.WriteString(p.field("Plan", plan.Action+" / "+plan.App))
		if notice := maintenanceDetailNotice(plan); notice != "" {
			b.WriteString(p.prose(notice))
		}
		if !r.apply && (r.action == "destroy" || r.action == "service-remove") {
			b.WriteString(p.prose("Applying this plan removes live resources. Retained data is not a backup."))
		}
		b.WriteString(p.section("Remove"))
		for _, group := range []struct {
			label  string
			values []string
		}{{"Containers", plan.Containers}, {"Images", plan.Images}, {"Archives", plan.Archives}} {
			if len(group.values) == 0 {
				empty := "(none)"
				if plan.DetailsIncomplete {
					empty = "(none displayed)"
				}
				b.WriteString(p.field(group.label, empty))
				continue
			}
			b.WriteString(p.ink("secondary", group.label, false) + "\n")
			for _, value := range group.values {
				b.WriteString("  " + displayText(value) + "\n")
			}
		}
		b.WriteString(p.field("Network", plan.Network))
		b.WriteString(p.section("Retain"))
		if len(plan.Retained) == 0 {
			if plan.DetailsIncomplete {
				b.WriteString(p.prose("No retained entries displayed."))
			} else {
				b.WriteString(p.prose("No retained resources listed in this plan."))
			}
		}
		for _, value := range plan.Retained {
			b.WriteString("  " + displayText(value) + "\n")
		}
	}
	if response.Status != nil {
		b.WriteString(p.retainedReport(response.Status, r.service))
	}
	if !r.apply && response.Plan != nil && r.approval != "" {
		b.WriteString(p.section("To apply"))
		b.WriteString(p.command(r.approval))
		b.WriteString(p.prose("The plan is recomputed under the host lock before applying."))
	}
	return b.String()
}
func (p *presentation) deploymentPreview(preview *deploy.DeploymentPreview, approval string, apply bool) string {
	text := p.section("Service update") + p.field("Service", preview.Service) + p.field("Current image", preview.CurrentImage) + p.field("Desired image", preview.DesiredImage) + p.field("Downtime", yesNo(preview.Downtime))
	if preview.NeedsConfirmation && !apply {
		text += "\n" + p.ink("warning", "Preview only: no update applied.", true) + "\n" + p.section("To apply") + p.command(approval)
	}
	return text
}
func (p *presentation) rollbackPreview(preview *deploy.RollbackPreview) string {
	text := p.section("Rollback") + p.field("Target operation", preview.TargetOperation)
	text += p.prose("Rollback restores code/configuration only, not data or schemas. Confirm the old code is compatible with current data.")
	for _, s := range preview.Services {
		text += p.section(s.Name)
		if s.ExcludedReason != "" {
			text += p.field("Excluded", s.ExcludedReason)
			continue
		}
		text += p.field("Current image", s.CurrentImage) + p.field("Target image", s.TargetImage) + p.field("Downtime", yesNo(s.Downtime))
		if len(s.CurrentDomains) > 0 || len(s.TargetDomains) > 0 {
			text += p.field("Current domains", fmt.Sprintf("%q", s.CurrentDomains)) + p.field("Target domains", fmt.Sprintf("%q", s.TargetDomains))
		}
		names := make([]string, 0, len(s.SecretVersions))
		for name := range s.SecretVersions {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			text += p.field("Captured secret · "+name, s.SecretVersions[name])
		}
		if len(names) > 0 {
			text += p.prose("Warning: captured credentials may have been revoked; rollback does not restore their validity.")
		}
	}
	if preview.NeedsConfirmation {
		text += "\n" + p.ink("warning", "Preview only", true) + "\n" + p.prose("Rerun rollback with --yes to apply; use --allow-downtime to authorize stop/start where required.")
	}
	return text
}

// Keep historical singleton output stable; include group detail during scale-up,
// scale-down and partial creation, even when only one current member remains.
func hasReplicaGroup(s deploy.ServiceStatus) bool {
	return s.DesiredInstances > 1 || s.CurrentInstances > 1 || len(s.Instances) > 1 || len(s.CandidateInstances) > 1
}

func (p *presentation) replicaReport(s deploy.ServiceStatus) string {
	if !hasReplicaGroup(s) {
		return ""
	}
	var b strings.Builder
	b.WriteString(p.field("Instances", fmt.Sprintf("Desired %d / Current %d / Running %d / Available %d", s.DesiredInstances, s.CurrentInstances, s.RunningInstances, s.AvailableInstances)))
	for _, instance := range s.Instances {
		b.WriteString(p.section(fmt.Sprintf("Replica %d", instance.Ordinal)))
		b.WriteString(p.field("Container", instance.Container))
		b.WriteString(p.field("Port", instance.Port))
		b.WriteString(p.field("Running", yesNo(instance.Running)))
		b.WriteString(p.field("Available", yesNo(instance.Available)))
	}
	for _, instance := range s.CandidateInstances {
		b.WriteString(p.section(fmt.Sprintf("Candidate replica %d", instance.Ordinal)))
		b.WriteString(p.field("Container", instance.Container))
		b.WriteString(p.field("Port", instance.Port))
		b.WriteString(p.field("Running", yesNo(instance.Running)))
	}
	return b.String()
}

func deploymentRouteNotice(s *deploy.Status) string {
	hasPublic, hasPorts := len(s.Services) == 0, false
	for _, ss := range s.Services {
		hasPublic = hasPublic || ss.Kind == "public"
		hasPorts = hasPorts || ss.Kind == "ports"
	}
	var messages []string
	if hasPublic {
		messages = append(messages, "Routes configured. Public DNS and certificate readiness are not implied by deployment success.")
	}
	if hasPorts {
		prefix := "Direct IPv4 ports configured."
		if !hasPublic {
			prefix = "Direct IPv4 ports configured; no public HTTP route."
		}
		messages = append(messages, prefix+" Protocol readiness and firewall reachability are not implied by deployment success.")
	}
	if len(messages) == 0 {
		return "Private-only app: no public route is configured."
	}
	return strings.Join(messages, "\n")
}

// Only acknowledged, recorded candidates are safe readiness log targets.
func readinessLogArgs(s *deploy.Status, selected string) []string {
	if s == nil {
		return nil
	}
	names := selectedServices(s, selected)
	for _, failedOnly := range []bool{true, false} {
		for _, name := range names {
			ss := s.Services[name]
			if failedOnly && !ss.Unavailable {
				continue
			}
			if ss.Candidate == "" && len(ss.CandidateInstances) == 0 {
				continue
			}
			args := []string{"--service", name, "--candidate"}
			if len(ss.CandidateInstances) > 1 {
				ordinal := ss.CandidateInstances[0].Ordinal
				for _, instance := range ss.CandidateInstances {
					if instance.Ordinal < ordinal {
						ordinal = instance.Ordinal
					}
				}
				args = append(args, "--replica", fmt.Sprint(ordinal))
			}
			return args
		}
	}
	return nil
}

func publishedPortSlots(ss deploy.ServiceStatus) []struct {
	name   string
	status *deploy.PublishedPortStatus
} {
	return []struct {
		name   string
		status *deploy.PublishedPortStatus
	}{{"current", ss.PublishedPorts}, {"candidate", ss.CandidatePublishedPorts}}
}

func publishedPortObservation(ports *deploy.PublishedPortStatus) string {
	if ports.Observed {
		return "bindings validated on running owned runtime"
	}
	return "bindings not validated; not confirmation that ports are closed"
}

func publishedPortCount(ports *deploy.PublishedPortStatus) string {
	text := fmt.Sprintf("Mappings listed: %d/%d", len(ports.Mappings), ports.Total)
	if len(ports.Mappings) < ports.Total {
		text += " (truncated)"
	}
	return text
}

func (p *presentation) publishedPortReport(service string, ss deploy.ServiceStatus) string {
	var b strings.Builder
	for _, slot := range publishedPortSlots(ss) {
		if slot.status == nil {
			continue
		}
		ports := slot.status
		b.WriteString(p.section(service + " · " + slot.name + " published ports"))
		b.WriteString(p.field("Recorded IPv4 bind", "0.0.0.0"))
		b.WriteString(p.field("Observed", yesNo(ports.Observed)))
		b.WriteString(p.prose(publishedPortObservation(ports)))
		b.WriteString(p.prose("HOST:CONTAINER[/protocol]; omitted protocol defaults to tcp"))
		for _, mapping := range ports.Mappings {
			b.WriteString(p.field("Mapping", mapping))
		}
		b.WriteString(p.prose(publishedPortCount(ports)))
		b.WriteString(p.prose("Public reachability is not verified."))
	}
	return b.String()
}
