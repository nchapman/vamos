package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/host"
	"github.com/nchapman/soar/internal/registry"
)

func helpAppsInventoryFixture() *deploy.AppList {
	return &deploy.AppList{Entries: []deploy.AppEntry{
		{App: "notes", Phase: "deployed", Services: []string{"web", "db"}, ServiceCount: 2, RetainedSecrets: true, VolumeRecords: 3, RetainedVolumes: false},
		{App: "gone", Phase: "destroyed", Services: []string{"archive"}, ServiceCount: 1, RetainedSecrets: false, VolumeRecords: 1, RetainedVolumes: true},
	}}
}

func TestAppsPresentationFieldsAndLayout(t *testing.T) {
	for _, tc := range []struct {
		name  string
		width int
		color bool
		table bool
	}{{"wide-color-table", 120, true, true}, {"narrow-color-records", 48, true, false}, {"narrow-mono-records", 48, false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			out := newHelpAppsTerminal(tc.width, tc.color)
			if err := out.p.write(out, out.p.renderApps("home", "root@home", helpAppsInventoryFixture())); err != nil {
				t.Fatal(err)
			}
			text := ansi.Strip(out.String())
			for _, want := range []string{"APPS", "home", "root@home", "Recorded inventory (not live health).", "Apps", "notes", "deployed", "web,db", "gone", "destroyed", "archive", "yes", "no", "3", "2", "1"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q in:\n%s", want, text)
				}
			}
			if tc.table {
				for _, want := range []string{"APP", "PHASE", "SERVICES (COUNT)", "RETAINED SECRETS", "VOLUME RECORDS", "RETAINED VOLUMES", "web,db (2)", "archive (1)"} {
					if !strings.Contains(text, want) {
						t.Errorf("missing table content %q in:\n%s", want, text)
					}
				}
			} else {
				for _, want := range []string{"Phase", "Services", "Service count", "Retained secrets", "Volume records", "Retained volumes"} {
					if strings.Count(text, want) != 2 {
						t.Errorf("expected two fields %q in:\n%s", want, text)
					}
				}
				if strings.Contains(text, "SERVICES (COUNT)") {
					t.Fatal("narrow output used table")
				}
			}
			if strings.Contains(out.String(), "\x1b[") != tc.color {
				t.Fatalf("color=%v output=%q", tc.color, out.String())
			}
			for _, line := range strings.Split(text, "\n") {
				if ansi.StringWidth(line) > tc.width {
					t.Errorf("line exceeds %d columns: %q", tc.width, line)
				}
			}
		})
	}
}

func TestAppsPresentationLongValuesUseRecordsAndEscapeControls(t *testing.T) {
	inventory := helpAppsInventoryFixture()
	entry := &inventory.Entries[0]
	entry.App = strings.Repeat("long-app-", 20)
	entry.Services = []string{strings.Repeat("service-", 30), "unsafe\x1b[2J\nservice"}
	entry.Phase = "phase\r\u202ehidden"
	out := newHelpAppsTerminal(120, false)
	if err := out.p.write(out, out.p.renderApps("home\x1b[2J", "root@host\r", inventory)); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{entry.App, entry.Services[0], "unsafe\\x1b[2J\\nservice", "phase\\r\\u202ehidden", "home\\x1b[2J", "root@host\\r", "Service count"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing complete escaped value %q in:\n%s", want, text)
		}
	}
	if strings.ContainsAny(text, "\x1b\r\u202e") || strings.Contains(text, "SERVICES (COUNT)") {
		t.Fatalf("unsafe controls or unfitted table: %q", text)
	}
}

func TestAppsPresentationEmptyInventory(t *testing.T) {
	out := newHelpAppsTerminal(48, false)
	text := out.p.renderApps("home", "root@home", &deploy.AppList{})
	for _, want := range []string{"Recorded inventory (not live health).", "Apps 0", "No registered apps"} {
		if !strings.Contains(strings.Join(strings.Fields(text), " "), want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, "PHASE") || strings.Contains(text, "healthy") {
		t.Fatalf("unexpected table or health claim: %q", text)
	}
}

func TestAppsPlainAndJSONContractsWithPresentation(t *testing.T) {
	_, store, reg := initFixture(t)
	if err := store.Put(registry.Server{Name: "home", Target: "root@home", Domain: "example.com"}); err != nil {
		t.Fatal(err)
	}
	inventory := helpAppsInventoryFixture()
	deps := appDeps{
		Registry: reg,
		Inspect: func(context.Context, string) (*host.Report, error) {
			return &host.Report{Root: true, Supported: true, Architecture: "amd64"}, nil
		},
		Artifact: func(string, string) ([]byte, error) { return []byte("fixture"), nil },
		Run: func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
			return deploy.Response{Version: 1, Apps: inventory}, nil
		},
	}
	const plain = "home (root@home): recorded inventory (not live health).\nAPP\tPHASE\tSERVICES (COUNT)\tRETAINED SECRETS\tVOLUME RECORDS\tRETAINED VOLUMES\nnotes\tdeployed\tweb,db (2)\ttrue\t3\tfalse\ngone\tdestroyed\tarchive (1)\tfalse\t1\ttrue\n"
	const jsonText = "{\"entries\":[{\"app\":\"notes\",\"phase\":\"deployed\",\"services\":[\"web\",\"db\"],\"service_count\":2,\"retained_secrets\":true,\"volume_records\":3,\"retained_volumes\":false},{\"app\":\"gone\",\"phase\":\"destroyed\",\"services\":[\"archive\"],\"service_count\":1,\"retained_secrets\":false,\"volume_records\":1,\"retained_volumes\":true}]}\n"
	var out, diag bytes.Buffer
	if code := appsCommand(context.Background(), nil, &out, &diag, deps); code != 0 || out.String() != plain || diag.Len() != 0 {
		t.Fatalf("plain code=%d stdout=%q stderr=%q", code, out.String(), diag.String())
	}
	out.Reset()
	if code := appsCommand(context.Background(), []string{"--json"}, &out, &diag, deps); code != 0 || out.String() != jsonText || diag.Len() != 0 {
		t.Fatalf("JSON code=%d stdout=%q stderr=%q", code, out.String(), diag.String())
	}
	terminal := newHelpAppsTerminal(120, true)
	if code := appsCommand(context.Background(), []string{"--json"}, terminal, &diag, deps); code != 0 || terminal.String() != jsonText || diag.Len() != 0 {
		t.Fatalf("terminal JSON code=%d stdout=%q stderr=%q", code, terminal.String(), diag.String())
	}
	terminal.Reset()
	if code := appsCommand(context.Background(), nil, terminal, &diag, deps); code != 0 || !strings.Contains(terminal.String(), "\x1b[") || diag.Len() != 0 {
		t.Fatalf("terminal text code=%d stdout=%q stderr=%q", code, terminal.String(), diag.String())
	}
	inventory = &deploy.AppList{Entries: []deploy.AppEntry{}}
	out.Reset()
	const empty = "home (root@home): recorded inventory (not live health).\nhome: no registered apps.\n"
	if code := appsCommand(context.Background(), nil, &out, &diag, deps); code != 0 || out.String() != empty || diag.Len() != 0 {
		t.Fatalf("empty plain code=%d stdout=%q stderr=%q", code, out.String(), diag.String())
	}
}
