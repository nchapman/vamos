package cli

import (
	"errors"
	"testing"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/firewall"
	"github.com/nchapman/vamos/internal/host"
)

// Firewall cases use the production writer boundary, not a parallel HTML view.
func featureVisualCases(t *testing.T, p *presentation, r appReport) map[string]string {
	t.Helper()
	cases := map[string]string{}
	for name, status := range map[string]*firewall.Status{
		"enabled":  {Enabled: true, SSHPorts: []int{22, 2222}},
		"disabled": {},
		"pending":  {Enabled: true, Pending: true, SSHPorts: []int{2222}},
		"degraded": {Enabled: true, Degraded: true, SSHPorts: []int{22}, Problems: []string{"managed service file changed", "unexpected reference to an owned firewall chain"}},
	} {
		out := &helpAppsTerminalBuffer{p: p}
		if err := renderFirewallStatus(out, r.server.Name, status); err != nil {
			t.Fatal(err)
		}
		cases["firewall-"+name] = out.String()
	}
	for _, op := range []string{"enable", "disable"} {
		out := &helpAppsTerminalBuffer{p: p}
		message := "cleanup interrupted after rule removal; inspect the remaining artifacts"
		if op == "enable" {
			message = "fresh SSH confirmation failed; automatic rollback remains armed unless confirmation reached the server"
		}
		firewallFailure(out, op, r.server.Name, r.server.Target, "", errors.New(message))
		cases["firewall-"+op+"-failure"] = out.String()
	}
	cases["server-sudo"] = p.renderServerInspect("ubuntu@home.example.net", &host.Report{OS: "ubuntu", Version: "24.04", Architecture: "arm64", Systemd: true, UID: 1000, Supported: true})
	r.action, r.mutation = "deploy", true
	status := &deploy.Status{App: r.app, Phase: "ready", Services: map[string]deploy.ServiceStatus{
		"game": {Kind: "ports", Phase: "ready", Running: true, Available: true, Container: "vamos-ledger-game-current", Image: "registry.example.net/game:v2"},
	}}
	cases["ports-success"] = p.header(r.action, r.app, r.server.Name, r.server.Target) + p.statusReport(status, r)
	status.Services["web"] = deploy.ServiceStatus{Kind: "public", Phase: "ready", Running: true, Available: true, URL: "https://ledger.example.net", Container: "vamos-ledger-web-current", Image: "registry.example.net/web:v2"}
	cases["mixed-success"] = p.header(r.action, r.app, r.server.Name, r.server.Target) + p.statusReport(status, r)
	failed := presentationStatusFixture()
	service := failed.Services["web"]
	service.Unavailable = true
	service.CandidateInstances = []deploy.InstanceStatus{{Ordinal: 1, Container: "vamos-ledger-web-candidate-1", Running: true}, {Ordinal: 2, Container: "vamos-ledger-web-candidate-2", Running: true}}
	failed.Services["web"] = service
	r.service = "web"
	err := errors.New("HTTP readiness: context deadline exceeded")
	cases["readiness-candidates"] = p.appFailure(r, deploy.Request{Operation: visualOperation}, deploy.Response{Version: 1, Error: err.Error(), Status: failed}, err, "")
	return cases
}
