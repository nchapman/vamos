package deploy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDomainJournalsRemainStrictAcrossVersions(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(statusDomainApp("alias.example.com"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	if j.Version != 6 {
		t.Fatal("domains require v6")
	}
	b, _ := json.Marshal(j)
	for _, version := range []int{1, 2, 3, 4, 5, 7} {
		var cp journal
		if err := json.Unmarshal(b, &cp); err != nil {
			t.Fatal(err)
		}
		cp.Version = version
		if err := validateJournal(&cp, cp.App); err == nil {
			t.Fatalf("aliases admitted by version %d", version)
		}
	}
	if _, err := f.deploy(statusDomainApp(), strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	if f.load().Version != 6 {
		t.Fatal("history lost domain schema")
	}
	if _, err := f.m.Execute(context.Background(), rollbackRequest(f, "3", "", true), nil); err != nil {
		t.Fatal(err)
	}
	if j = f.load(); j.Version != 6 || j.Rollback == nil || len(j.ServiceStates["web"].Current.Config.HTTP.Domains) != 1 {
		t.Fatal("domain rollback failed")
	}
}
func TestDomainRendererRejectsUnsafeHostnames(t *testing.T) {
	for _, host := range []string{"bad.example.com {", "https://example.com", "*.example.com", "127.0.0.1", "example.com\nrespond 200", "example.com:443"} {
		cfg := statusDomainApp(host).Services["web"]
		if _, err := serviceHostnames("notes", "web", "example.com", &cfg); err == nil {
			t.Fatalf("unsafe hostname %q", host)
		}
	}
	cfg := statusDomainApp("NOTES.example.com", "alias.example.com").Services["web"]
	hosts, err := serviceHostnames("notes", "web", "example.com", &cfg)
	if err != nil || len(hosts) != 2 {
		t.Fatalf("own generated alias not deduplicated: %v %v", hosts, err)
	}
}
