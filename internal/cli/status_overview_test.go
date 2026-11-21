package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/registry"
)

func TestStatusOutsideProjectListsLocalServers(t *testing.T) {
	dir, store, reg := initFixture(t)
	t.Chdir(dir)
	deps := appDeps{Registry: reg} // No transport: the overview must also work offline.
	var out, diag bytes.Buffer
	status := func(args ...string) string {
		t.Helper()
		out.Reset()
		if code := appCommand(t.Context(), "status", args, &out, &diag, deps); code != 0 {
			t.Fatalf("status %v: %d: %s", args, code, &diag)
		}
		return out.String()
	}
	if got := status(); !strings.Contains(got, "No registered servers") || !strings.Contains(got, "vamos server connect") {
		t.Fatal(got)
	}
	for _, name := range []string{"west", "east"} {
		if err := store.Put(registry.Server{Name: name, Target: "root@" + name, Domain: name + ".example.org"}); err != nil {
			t.Fatal(err)
		}
	}
	got := status()
	for _, want := range []string{"west (default)", "root@east", "west.example.org", "vamos status --server west"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
	var overview struct {
		Default string
		Servers []registry.Server
	}
	if err := json.Unmarshal([]byte(status("--json")), &overview); err != nil || overview.Default != "west" || len(overview.Servers) != 2 || overview.Servers[0].Name != "east" {
		t.Fatalf("overview = %+v, error = %v", overview, err)
	}
}

func TestStatusOutsideProjectSelectsServerInventory(t *testing.T) {
	dir, store, reg := initFixture(t)
	t.Chdir(dir)
	for _, name := range []string{"home", "example"} {
		if err := store.Put(registry.Server{Name: name, Target: "root@" + name, Domain: name + ".example.org"}); err != nil {
			t.Fatal(err)
		}
	}
	deps := appDeps{Registry: reg, Agent: func(ctx context.Context, target string, req deploy.Request, _ []byte) (deploy.Response, error) {
		if _, ok := ctx.Deadline(); !ok || target != "root@example" || req.Host.Name != "example" || req.Action != "apps" {
			t.Fatalf("unexpected inventory request: %s %+v", target, req)
		}
		return deploy.Response{Version: 1, Apps: &deploy.AppList{Entries: []deploy.AppEntry{{App: "hello", Phase: "ready", Services: []string{"web"}, ServiceCount: 1}}}}, nil
	}}
	var out, diag bytes.Buffer
	args := []string{"--server", "example"}
	if code := appCommand(t.Context(), "status", args, &out, &diag, deps); code != 0 || !strings.Contains(out.String(), "not live health") || !strings.Contains(out.String(), "hello") || !strings.Contains(out.String(), "vamos status --server example --app") {
		t.Fatalf("%d: %s %s", code, &out, &diag)
	}
	out.Reset()
	var apps deploy.AppList
	if code := appCommand(t.Context(), "status", append(args, "--json"), &out, &diag, deps); code != 0 || json.Unmarshal(out.Bytes(), &apps) != nil || len(apps.Entries) != 1 || apps.Entries[0].App != "hello" {
		t.Fatalf("%d: %s %s", code, &out, &diag)
	}
	if _, err := os.Stat(".vamos"); !os.IsNotExist(err) {
		t.Fatalf("unexpected project metadata: %v", err)
	}
}

func TestStatusDoesNotMaskManifestOrAppSelectorErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	check := func(args ...string) {
		t.Helper()
		var out, diag bytes.Buffer
		if code := appCommand(t.Context(), "status", args, &out, &diag, appDeps{}); code == 0 || !strings.Contains(diag.String(), config.DefaultFile) || out.Len() != 0 {
			t.Fatalf("%v: %d: %s %s", args, code, &out, &diag)
		}
	}
	check("-f", config.DefaultFile)
	check("--service", "web")
	if err := os.WriteFile(config.DefaultFile, []byte("invalid: ["), 0600); err != nil {
		t.Fatal(err)
	}
	check()
}
