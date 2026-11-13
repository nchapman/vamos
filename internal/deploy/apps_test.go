package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/appstate"
	"github.com/nchapman/soar/internal/process"
	"github.com/nchapman/soar/internal/volumes"
)

func inventoryRequest(r Request) Request { return Request{Version: 1, Action: "apps", Host: r.Host} }
func inventoryNoProbes(t *testing.T, m *Manager) {
	t.Helper()
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		t.Fatal("inventory ran process")
		return process.Result{}, nil
	}
	m.DomainProbe = func(context.Context, string) DomainStatus { t.Fatal("inventory probed domain"); return DomainStatus{} }
}
func inventoryTombstone(t *testing.T, m *Manager, app, hostID string) {
	t.Helper()
	data, _ := json.Marshal(appstate.Tombstone{Version: 1, App: app, HostID: hostID, Phase: "destroyed", Operation: strings.Repeat("c", 32)})
	if err := atomic(filepath.Join(m.dir(app), "state.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestAppsEmptyStagedDestroyedAndRetained(t *testing.T) {
	m, r := setup(t)
	inventoryNoProbes(t, m)
	q := inventoryRequest(r)
	response, err := m.Execute(context.Background(), q, nil)
	if err != nil || response.Apps == nil || response.Apps.Entries == nil || len(response.Apps.Entries) != 0 {
		t.Fatalf("empty: %+v %v", response, err)
	}
	stageSecret(t, m, r, "1", secretCanary)
	hostID := strings.Repeat("a", 32)
	inventoryTombstone(t, m, "gone", hostID)
	inventoryTombstone(t, m, "empty-gone", hostID)
	for _, name := range []string{"gone", "volume-only"} {
		rec := volumes.Record{ID: strings.Repeat("d", 32), Service: "db", Name: "data", Mount: "/data", Operation: strings.Repeat("c", 32), State: "created", CreatedAt: "2026-01-01T00:00:00Z"}
		if err := m.volumeStore().Save(&volumes.State{Version: 1, App: name, HostID: hostID, Records: map[string]volumes.Record{"db": rec}}); err != nil {
			t.Fatal(err)
		}
	}
	response, err = m.Execute(context.Background(), q, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries := response.Apps.Entries
	if len(entries) != 4 || entries[0].App != "empty-gone" || entries[1].App != "gone" || entries[2].App != "notes" || entries[3].App != "volume-only" {
		t.Fatalf("not sorted: %+v", entries)
	}
	if entries[0].Phase != "destroyed" || entries[0].RetainedVolumes || entries[1].Phase != "destroyed" || !entries[1].RetainedVolumes || entries[1].VolumeRecords != 1 || entries[1].ServiceCount != 1 || entries[1].Services[0] != "db" {
		t.Fatalf("destroyed: %+v", entries)
	}
	if entries[2].Phase != "staged" || !entries[2].RetainedSecrets || entries[2].ServiceCount != 0 || entries[3].Phase != "retained" {
		t.Fatalf("retained: %+v", entries)
	}
	data, _ := json.Marshal(response)
	if strings.Contains(string(data), "not-a-real-credential") {
		t.Fatal("secret exposed")
	}
	again, err := m.Execute(context.Background(), q, nil)
	next, _ := json.Marshal(again)
	if err != nil || string(data) != string(next) {
		t.Fatal("nondeterministic response", err)
	}
}
func TestAppsJournalServicesAndNoRuntimeProbes(t *testing.T) {
	m, r := setup(t)
	j := &journal{Version: 1, HostID: strings.Repeat("a", 32), Status: Status{App: r.App, URL: "https://notes.example.com", Phase: "building", Operation: r.Operation}, Config: r.Config, Archive: r.Operation + ".context.tar"}
	if err := m.save(j); err != nil {
		t.Fatal(err)
	}
	inventoryNoProbes(t, m)
	response, err := m.Execute(context.Background(), inventoryRequest(r), nil)
	if err != nil {
		t.Fatal(err)
	}
	entry := response.Apps.Entries[0]
	if entry.Phase != "building" || entry.ServiceCount != 1 || entry.Services[0] != "web" {
		t.Fatalf("%+v", entry)
	}
}
func TestAppsFailClosedOnBadMetadata(t *testing.T) {
	for _, kind := range []string{"foreign-journal", "corrupt-journal", "missing-journal", "symlink-journal", "foreign-secret", "corrupt-secret", "foreign-volume", "unexpected-file", "unsafe-directory", "host-config", "host-owner-mode"} {
		t.Run(kind, func(t *testing.T) {
			m, r := setup(t)
			inventoryNoProbes(t, m)
			q := inventoryRequest(r)
			switch kind {
			case "foreign-journal":
				inventoryTombstone(t, m, "gone", strings.Repeat("b", 32))
			case "corrupt-journal":
				if err := atomic(filepath.Join(m.dir("bad"), "state.json"), []byte(`{"unknown-secret-canary":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-journal":
				if err := os.MkdirAll(m.dir("bad"), 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink-journal":
				inventoryTombstone(t, m, "gone", strings.Repeat("a", 32))
				path := filepath.Join(m.dir("gone"), "state.json")
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".old", path); err != nil {
					t.Skip(err)
				}
			case "foreign-secret", "corrupt-secret":
				stageSecret(t, m, r, "1", secretCanary)
				path := m.path("/var/lib/soar/secrets/notes.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "foreign-secret" {
					data = []byte(strings.ReplaceAll(string(data), strings.Repeat("a", 32), strings.Repeat("b", 32)))
				} else {
					data = []byte(`{"value":"secret-canary"}`)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-volume":
				if err := m.volumeStore().Save(&volumes.State{Version: 1, App: "bad", HostID: strings.Repeat("b", 32), Records: map[string]volumes.Record{}}); err != nil {
					t.Fatal(err)
				}
			case "unexpected-file":
				if err := atomic(m.path("/var/lib/soar/secrets/not-json"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unsafe-directory":
				inventoryTombstone(t, m, "gone", strings.Repeat("a", 32))
				if err := os.Chmod(m.dir("gone"), 0777); err != nil {
					t.Fatal(err)
				}
			case "host-config":
				q.Host.Domain = "other.example.com"
			case "host-owner-mode":
				if err := os.Chmod(m.path("/var/lib/soar/host.json"), 0666); err != nil {
					t.Fatal(err)
				}
			}
			response, err := m.Execute(context.Background(), q, nil)
			if err == nil || response.Apps != nil {
				t.Fatalf("accepted %s: %+v %v", kind, response, err)
			}
			if strings.Contains(response.Error, "secret-canary") {
				t.Fatal("metadata error leaked content")
			}
		})
	}
}
func TestAppsEnumerationAndEnvelopeBounds(t *testing.T) {
	m, r := setup(t)
	for i := 0; i <= MaxApps; i++ {
		inventoryTombstone(t, m, fmt.Sprintf("app-%03d", i), strings.Repeat("a", 32))
	}
	response, err := m.Execute(context.Background(), inventoryRequest(r), nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds") || response.Apps != nil {
		t.Fatal("enumeration not bounded", err)
	}
	m, r = setup(t)
	// Valid metadata can still exceed the RPC envelope; it must not be truncated.
	for i := 0; i < MaxApps; i++ {
		name := fmt.Sprintf("app-%03d-%s", i, strings.Repeat("x", 55))
		recs := map[string]volumes.Record{}
		for j := 0; j < 4; j++ {
			service := fmt.Sprintf("service-%d-%s", j, strings.Repeat("x", 50))
			recs[service] = volumes.Record{ID: fmt.Sprintf("%032x", j+1), Service: service, Name: service, Mount: "/data", Operation: strings.Repeat("b", 32), State: "planned"}
		}
		if err := m.volumeStore().Save(&volumes.State{Version: 1, App: name, HostID: strings.Repeat("a", 32), Records: recs}); err != nil {
			t.Fatal(err)
		}
	}
	response, err = m.Execute(context.Background(), inventoryRequest(r), nil)
	if err == nil || !strings.Contains(err.Error(), "64 KiB") || response.Apps != nil {
		t.Fatal("response not bounded", err)
	}
}
func TestAppsRequestContract(t *testing.T) {
	_, r := setup(t)
	q := inventoryRequest(r)
	if err := ValidateRequest(q); err != nil || q.Mutating() {
		t.Fatal(err)
	}
	for _, change := range []func(*Request){func(r *Request) { r.App = "notes" }, func(r *Request) { r.Config = app() }, func(r *Request) { r.Operation = strings.Repeat("a", 32) }, func(r *Request) { r.Apply = true }, func(r *Request) { r.Service = "web" }, func(r *Request) { r.Tail = 1 }, func(r *Request) { r.HostIdentity = strings.Repeat("a", 32) }} {
		bad := q
		change(&bad)
		if ValidateRequest(bad) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
