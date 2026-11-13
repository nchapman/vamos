package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

func TestDomainPendingReloadIsNotReportedAsConfiguredAndResumes(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(statusDomainApp("old.example.com"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	base := f.m.Run
	fail := true
	f.m.Run = func(ctx context.Context, n string, a []string, b []byte) (process.Result, error) {
		if n == "caddy" && len(a) > 0 && a[0] == "reload" && fail {
			return process.Result{}, errors.New("injected reload outage")
		}
		return base(ctx, n, a, b)
	}
	if _, err := f.deploy(statusDomainApp("new.example.com"), strings.Repeat("2", 32)); err == nil {
		t.Fatal("reload failure ignored")
	}
	if f.load().NewCaddy == "" {
		t.Fatal("missing durable route intent")
	}
	f.m.DomainProbe = func(context.Context, string) DomainStatus {
		t.Error("uncertain routes must not be probed as configured")
		return DomainStatus{DNS: "matches-host", Certificate: "ready"}
	}
	q := f.req
	q.Action = "status"
	q.Operation = ""
	q.Config = nil
	q.ContextFormat = ""
	resp, err := f.m.Execute(context.Background(), q, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Status.Services["web"].Domains {
		if d.Route != "pending" || d.HTTPS != "unknown" {
			t.Fatalf("uncertain route presented as ready: %+v", d)
		}
	}
	fail = false
	if _, err = f.resume(strings.Repeat("2", 32)); err != nil {
		t.Fatal(err)
	}
	text, err := f.m.routes(f.load(), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "old.example.com") || !strings.Contains(text, "new.example.com") || !strings.Contains(text, "notes.example.com") {
		t.Fatal(text)
	}
}
