package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/registry"
	"github.com/nchapman/vamos/internal/secrets"
)

const visualOperation = "5ec713c03bfd4d64867f6edcb7da0970"

func presentationStatusFixture() *deploy.Status {
	return &deploy.Status{App: "ledger", Phase: "updating", Operation: visualOperation, Services: map[string]deploy.ServiceStatus{
		"web":      {Kind: "public", Phase: "ready", Running: true, Available: true, URL: "https://ledger.example.net", Operation: visualOperation, Container: "vamos-ledger-web-current", Image: "registry.example.net/ledger:v7", Domains: []deploy.DomainStatus{{Hostname: "www.ledger.example.net", Route: "configured", DNS: "pending", Certificate: "pending", HTTPS: "pending"}}},
		"database": {Kind: "private", Phase: "starting", Unavailable: true, DataMayHaveChanged: true, Operation: visualOperation, Container: "vamos-ledger-database-retired", Candidate: "vamos-ledger-database-candidate", Image: "postgres:17"},
	}, Volumes: []deploy.VolumeStatus{{Service: "database", Name: "ledger-data", Mount: "/var/lib/postgresql/data", ID: "volume-identity-0123456789", State: "attached", DataMayHaveChanged: true}}, Secrets: []deploy.SecretStatus{{Name: "database-password", StagedVersion: "secret-version-new", AppliedVersion: "secret-version-old", Pending: true, Services: []string{"web", "database"}, AppliedVersions: map[string]string{"web": "secret-version-old", "database": "secret-version-new"}}}}
}
func presentationReportFixture() appReport {
	return appReport{action: "status", app: "ledger", server: registry.Server{Name: "home", Target: "root@home.example.net", Domain: "example.net"}}
}
func TestPlainReadinessFailureProvidesCandidateRecovery(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		err := errors.New("HTTP readiness: context deadline exceeded")
		return deploy.Response{Version: 1, Error: err.Error(), Status: presentationStatusFixture()}, err
	}
	var out, diagnostic bytes.Buffer
	code := appCommand(context.Background(), "resume", []string{"-f", path, visualOperation}, &out, &diagnostic, deps)
	if code != 1 {
		t.Fatalf("code=%d: %s", code, &diagnostic)
	}
	for _, want := range []string{readinessRecovery, "Inspect candidate: vamos logs", "--candidate"} {
		if !strings.Contains(diagnostic.String(), want) {
			t.Fatalf("missing %q: %s", want, &diagnostic)
		}
	}
}

func TestReadinessFailureExplainsRetainedCandidateWithoutAvailabilityPromise(t *testing.T) {
	r := presentationReportFixture()
	r.action, r.mutation = "deploy", true
	err := errors.New("HTTP readiness: context deadline exceeded")
	request := deploy.Request{Operation: visualOperation}
	response := deploy.Response{Version: 1, Error: err.Error(), Status: presentationStatusFixture()}
	text := strings.Join(strings.Fields(newHelpAppsTerminal(120, false).p.appFailure(r, request, response, err, "")), " ")
	for _, want := range []string{"Readiness recovery", "stop-first services may be unavailable", "--supersede OPERATION", "vamos logs --server home --app ledger --service database --candidate", "vamos resume"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
}

func TestStatusPresentationPreservesFactsAndRecoveryOrder(t *testing.T) {
	for _, width := range []int{48, 80, 120} {
		out := newHelpAppsTerminal(width, true)
		s := presentationStatusFixture()
		if err := out.p.write(out, out.p.statusReport(s, presentationReportFixture())); err != nil {
			t.Fatal(err)
		}
		text := ansi.Strip(out.String())
		for _, want := range []string{"STATUS", "ledger", "home", "root@home.example.net", "Needs attention", "updating", visualOperation, "Host operation active", "Running", "Available", "Unavailable", "Data may have changed", "vamos-ledger-web-current", "registry.example.net/ledger:v7", "vamos-ledger-database-candidate", "ledger-data", "/var/lib/postgresql/data", "volume-identity-0123456789", "attached", "database-password", "secret-version-new", "secret-version-old", "Pending", "www.ledger.example.net", "DNS", "Certificate", "HTTPS"} {
			// Wide tables use uppercase headings; values must remain exact.
			if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
				t.Errorf("width %d missing %q in %s", width, want, text)
			}
		}
		command := "vamos logs --server home --app ledger --service database --candidate"
		if !strings.Contains(text, command) || strings.Index(text, command) > strings.Index(text, "Services\n") {
			t.Fatalf("recovery buried or changed: %s", text)
		}
		if strings.Contains(text, "healthy") {
			t.Fatal("invented health assertion")
		}
	}
}
func TestStatusPresentationDoesNotInvertAvailabilityOrLeakOtherServices(t *testing.T) {
	s := presentationStatusFixture()
	ss := s.Services["web"]
	ss.Running = false
	ss.Available = false
	ss.Unavailable = false
	s.Services["web"] = ss
	r := presentationReportFixture()
	r.service = "web"
	p := newHelpAppsTerminal(48, false).p
	text := p.statusReport(s, r)
	if !strings.Contains(text, "Available no / Unavailable no") {
		t.Fatal(text)
	}
	for _, unexpected := range []string{"Needs attention", "vamos-ledger-database-candidate", "ledger-data", "rollback does not restore data"} {
		if strings.Contains(text, unexpected) {
			t.Fatalf("unselected service output %q", unexpected)
		}
	}
	if !strings.Contains(text, "database-password") {
		t.Fatal("shared secret metadata lost")
	}
}
func TestStyledAppStatusAndJSONUseCorrectOutputPaths(t *testing.T) {
	path, _, deps := appFixture(t)
	status := presentationStatusFixture()
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		return deploy.Response{Version: 1, Status: status}, nil
	}
	for _, asJSON := range []bool{false, true} {
		out := newHelpAppsTerminal(80, true)
		var diagnostic bytes.Buffer
		args := []string{"-f", path}
		if asJSON {
			args = append(args, "--json")
		}
		if code := appCommand(context.Background(), "status", args, out, &diagnostic, deps); code != 0 || diagnostic.Len() != 0 {
			t.Fatalf("code=%d err=%s", code, &diagnostic)
		}
		if asJSON {
			var expected bytes.Buffer
			if err := json.NewEncoder(&expected).Encode(status); err != nil {
				t.Fatal(err)
			}
			if out.String() != expected.String() {
				t.Fatal("JSON changed by presentation")
			}
		} else if !strings.Contains(out.String(), "\x1b[") || !strings.Contains(ansi.Strip(out.String()), "Needs attention") {
			t.Fatal(out.String())
		}
	}
}
func TestStyledRawLogsAndRunOutputRemainVerbatim(t *testing.T) {
	path, _, deps := appFixture(t)
	raw := "raw\x1b[31mred\x1b[0m\r\n\x00payload"
	for _, action := range []string{"logs", "run"} {
		deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
			if action == "logs" {
				return deploy.Response{Version: 1, Logs: raw, LogService: "web", LogContainer: "pinned"}, nil
			}
			code := 7
			return deploy.Response{Version: 1, Run: &deploy.RunResult{Output: raw, ExitCode: &code}}, nil
		}
		out := newHelpAppsTerminal(48, true)
		diag := newHelpAppsTerminal(48, true)
		args := []string{"-f", path}
		if action == "run" {
			args = append(args, "--service", "web", "--", "echo", "test")
		}
		code := appCommand(context.Background(), action, args, out, diag, deps)
		if action == "logs" && (code != 0 || out.String() != raw) {
			t.Fatalf("logs rewritten: %q, %d", out.String(), code)
		}
		if action == "run" && (code != 7 || !strings.Contains(out.String(), raw)) {
			t.Fatalf("run output changed: %q, %d", out.String(), code)
		}
	}
}
func TestStyledFailureDistinguishesRefusalAndUnknown(t *testing.T) {
	p := newHelpAppsTerminal(48, false).p
	r := presentationReportFixture()
	r.action = "deploy"
	r.mutation = true
	request := deploy.Request{Operation: visualOperation}
	unknown := p.appFailure(r, request, deploy.Response{}, errors.New("connection ended"), "")
	if !strings.Contains(unknown, "Outcome unknown") || !strings.Contains(unknown, "vamos status --server home --app ledger") || strings.Contains(unknown, "service bind") {
		t.Fatal(unknown)
	}
	known := p.appFailure(r, request, deploy.Response{Version: 1, Error: "refused"}, errors.New("request refused"), "")
	if strings.Contains(known, "Outcome unknown") || !strings.Contains(known, "Request failed") {
		t.Fatal(known)
	}
}
func TestMaintenancePresentationPreservesExactPlanAndDoesNotInventApproval(t *testing.T) {
	p := newHelpAppsTerminal(48, false).p
	r := presentationReportFixture()
	r.action = "destroy"
	r.mutation = true
	plan := &deploy.Plan{Action: "destroy", App: "ledger", Containers: []string{"container-full-identity"}, Images: []string{"sha256:" + strings.Repeat("a", 64)}, Archives: []string{"/owned/archive-with-long-name"}, Network: "network-full-identity", Retained: []string{"volume: retained-data", "secrets: all versions"}}
	text := p.maintenanceReport(deploy.Response{Plan: plan}, r)
	for _, value := range []string{plan.Containers[0], plan.Images[0], plan.Archives[0], plan.Network, plan.Retained[0], plan.Retained[1], "Preview only: no resources deleted."} {
		if !strings.Contains(text, value) {
			t.Fatal(value, text)
		}
	}
	if strings.Contains(text, "--yes") {
		t.Fatal("invented approval bypassing protection flags")
	}
	r.action = "status"
	r.mutation = false
	if text := p.maintenanceReport(deploy.Response{Plan: plan}, r); strings.Contains(text, "Maintenance completed") {
		t.Fatal("recorded plan called completed")
	}
}
func TestCapturedApprovalPinsTargetAndPreservesScopeAndAuthorization(t *testing.T) {
	for _, action := range []string{"destroy", "cleanup", "rollback", "service-remove"} {
		f := flag.NewFlagSet(action, flag.ContinueOnError)
		f.String("service", "", "")
		f.String("executor", "", "")
		f.Bool("allow-protected", false, "")
		f.Bool("allow-downtime", false, "")
		args := []string{"--executor", "custom executor"}
		if action == "rollback" {
			args = append(args, "--service", "database", "--allow-downtime=false")
		}
		if action == "service-remove" {
			args = append(args, "--service", "database", "--allow-protected=false")
		}
		if err := f.Parse(args); err != nil {
			t.Fatal(err)
		}
		command := capturedApproval(f, action, "staging", "notes")
		for _, want := range []string{"--server staging --app notes --yes", "--executor 'custom executor'"} {
			if !strings.Contains(command, want) {
				t.Fatal(command)
			}
		}
		if action == "rollback" && (!strings.Contains(command, "--service database") || !strings.Contains(command, "--allow-downtime=false")) {
			t.Fatal(command)
		}
		if action == "service-remove" && (!strings.HasPrefix(command, "vamos service remove database ") || !strings.Contains(command, "--allow-protected=false")) {
			t.Fatal(command)
		}
	}
}

func TestChangedDataAloneIsNotReportedAsAFailedService(t *testing.T) {
	s := presentationStatusFixture()
	service := s.Services["database"]
	service.Unavailable = false
	service.Running, service.Available = true, true
	s.Services["database"] = service
	text := newHelpAppsTerminal(80, false).p.statusReport(s, presentationReportFixture())
	if strings.Contains(text, "Needs attention") {
		t.Fatal("historical data-change flag turned into an alarm")
	}
	if !strings.Contains(text, "Data may have changed") {
		t.Fatal("data-change qualification lost")
	}
}

func TestPresentationVisualFixtures(t *testing.T) {
	dir := os.Getenv("VAMOS_VISUAL_DIR")
	if dir == "" {
		t.Skip("set VAMOS_VISUAL_DIR to capture production presentation fixtures")
	}
	type fixture struct {
		Case  string `json:"case"`
		Theme string `json:"theme"`
		Width int    `json:"width"`
		ANSI  string `json:"ansi"`
	}
	var fixtures []fixture
	for _, width := range []int{48, 80, 120} {
		for _, theme := range []string{"dark", "light", "mono", "terminal"} {
			p := newHelpAppsTerminal(width, theme != "mono").p
			p.theme = theme
			status := presentationStatusFixture()
			r := presentationReportFixture()
			previewReport := r
			previewReport.action = "destroy"
			previewReport.mutation = true
			previewReport.approval = capturedApproval(flag.NewFlagSet("destroy", flag.ContinueOnError), "destroy", r.server.Name, r.app)
			prefix := p.header("destroy", r.app, r.server.Name, r.server.Target) + p.field("Requested operation", visualOperation) + "\n"
			plan := &deploy.Plan{Action: "destroy", App: r.app, Containers: []string{"vamos-ledger-web-" + visualOperation, "vamos-ledger-database-" + visualOperation}, Images: []string{"sha256:" + strings.Repeat("a", 64)}, Archives: []string{"/var/lib/vamos/apps/ledger/context-" + visualOperation + ".tar"}, Network: "vamos-ledger", Retained: []string{"volume ledger-data", "all secret versions"}}
			failureReport := r
			failureReport.action = "deploy"
			failureReport.mutation = true
			cases := map[string]string{
				"status":   p.statusReport(status, r),
				"server":   p.renderServerInspect(r.server.Target, &host.Report{OS: "ubuntu", Version: "24.04", Kernel: "Linux", Architecture: "arm64", Systemd: true, Root: true, Supported: true}),
				"secrets":  p.renderSecretList(r.server.Name, r.server.Target, r.app, []secrets.Entry{{Name: "database-password", Version: visualOperation, Latest: true}}),
				"preview":  prefix + p.maintenanceReport(deploy.Response{Plan: plan}, previewReport),
				"unknown":  p.appFailure(failureReport, deploy.Request{Operation: visualOperation}, deploy.Response{}, errors.New("SSH connection ended before an acknowledgement arrived"), ""),
				"help":     p.renderMainHelp(),
				"apps":     p.renderApps(r.server.Name, r.server.Target, helpAppsInventoryFixture()),
				"rollback": p.header("rollback", r.app, r.server.Name, r.server.Target) + p.rollbackPreview(&deploy.RollbackPreview{TargetOperation: visualOperation, NeedsConfirmation: true, Services: []deploy.RollbackService{{Name: "web", CurrentImage: "registry.example.net/ledger:v7", TargetImage: "registry.example.net/ledger:v6", Downtime: true, SecretVersions: map[string]string{"database-password": "secret-version-old"}}, {Name: "database", ExcludedReason: "manual service excluded"}}}),
				"staged":   p.statusReport(&deploy.Status{App: r.app, Phase: "staged", Secrets: status.Secrets}, r),
			}
			replicated := presentationStatusFixture()
			replicated.Phase = "ready"
			database := replicated.Services["database"]
			database.Phase, database.Candidate = "ready", ""
			database.Running, database.Available, database.Unavailable = true, true, false
			replicated.Services["database"] = database
			group := replicated.Services["web"]
			group.Container = ""
			group.DesiredInstances, group.CurrentInstances, group.RunningInstances, group.AvailableInstances = 2, 2, 2, 2
			group.Instances = []deploy.InstanceStatus{
				{Ordinal: 1, Container: "vamos-ledger-web-" + visualOperation + "-1", Port: "24001", Running: true, Available: true},
				{Ordinal: 2, Container: "vamos-ledger-web-" + visualOperation + "-2", Port: "24002", Running: true, Available: true},
			}
			replicated.Services["web"] = group
			zero := 0
			replicated.Release = &deploy.RunResult{Operation: strings.Repeat("a", 32), ParentOperation: visualOperation, Service: "web", Phase: "complete", ExitCode: &zero}
			cases["replicas-release"] = p.statusReport(replicated, r)
			failed := 23
			replicated.Phase = "releasing"
			group.Phase = "pending"
			replicated.Services["web"] = group
			replicated.LastError = "Release command failed with exit code 23; existing runtimes are unchanged."
			replicated.Release.ExitCode = &failed
			cases["release-blocked"] = p.statusReport(replicated, r)
			r.app = "customer-notification-service"
			r.server.Target = "root@deployment-host-with-a-long-name.example.net"
			status.App = r.app
			web := status.Services["web"]
			web.URL = "https://customer-notifications.production.example.net"
			web.Image = "sha256:" + strings.Repeat("e", 64)
			status.Services["web"] = web
			cases["long-status"] = p.statusReport(status, r)
			for name, text := range featureVisualCases(t, p, presentationReportFixture()) {
				cases[name] = text
			}
			required := []string{"status", "long-status", "preview", "unknown", "help", "apps", "rollback", "staged", "server", "secrets", "replicas-release", "release-blocked", "firewall-enabled", "firewall-disabled", "firewall-pending", "firewall-degraded", "firewall-enable-failure", "firewall-disable-failure", "server-sudo", "readiness-candidates", "ports-success", "mixed-success"}
			if len(cases) != len(required) {
				t.Fatalf("visual cases=%d required=%d", len(cases), len(required))
			}
			for _, name := range required {
				if strings.TrimSpace(cases[name]) == "" {
					t.Fatalf("missing visual case %s/%s/%d", name, theme, width)
				}
				var out bytes.Buffer
				if err := p.write(&out, cases[name]); err != nil {
					t.Fatal(err)
				}
				fixtures = append(fixtures, fixture{name, theme, width, out.String()})
			}
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(fixtures, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures.json"), append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}
