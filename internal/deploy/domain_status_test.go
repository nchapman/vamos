package deploy

import (
	"context"
	"net"
	"strings"
	syncatomic "sync/atomic"
	"testing"

	"github.com/nchapman/soar/internal/config"
)

func TestClassifyDomainDNS(t *testing.T) {
	public := net.ParseIP("8.8.8.8")
	for _, tc := range []struct {
		name string
		ips  []net.IP
		want string
	}{
		{"missing", nil, "pending"}, {"matching", []net.IP{public}, "matches-host"},
		{"wrong", []net.IP{net.ParseIP("1.1.1.1")}, "unverified"},
		{"mixed", []net.IP{public, net.ParseIP("1.1.1.1")}, "unverified"},
		{"loopback", []net.IP{net.ParseIP("127.0.0.1")}, "unverified"},
		{"private", []net.IP{net.ParseIP("10.0.0.1")}, "unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyDomainDNS(tc.ips, []net.IP{public, net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.1")}); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
func statusDomainApp(aliases ...string) *config.App {
	a := multiApp()
	s := a.Services["web"]
	s.HTTP.Domains = aliases
	a.Services["web"] = s
	return a
}
func TestDomainEnrichmentUsesCapturedStateNotUnselectedLocalEdits(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(statusDomainApp("OLD.example.com"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	resp := Response{Status: &Status{Services: map[string]ServiceStatus{"web": {Kind: "public"}}}}
	r := f.req
	r.Config = statusDomainApp("uncaptured.example.com")
	f.m.enrichDomainResponse(context.Background(), r, &resp)
	ds := resp.Status.Services["web"].Domains
	if len(ds) != 1 || ds[0].Hostname != "old.example.com" {
		t.Fatalf("invented local pending edits: %+v", ds)
	}
}
func TestDomainEnrichmentSelectionAndReadiness(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(statusDomainApp("OLD.example.com"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	f.failHealth["web"] = true
	if _, err := f.deploy(statusDomainApp("new.example.com"), strings.Repeat("2", 32)); err == nil {
		t.Fatal("expected readiness failure")
	}
	var calls syncatomic.Int32
	f.m.DomainProbe = func(context.Context, string) DomainStatus {
		calls.Add(1)
		return DomainStatus{DNS: "matches-host", Certificate: "pending"}
	}
	for _, tc := range []struct {
		action, selected string
		active           bool
		probes           int32
	}{
		{"deploy", "", false, 0}, {"status", "backend", false, 0}, {"status", "", true, 0}, {"status", "", false, 1},
	} {
		calls.Store(0)
		resp := Response{Status: &Status{Active: tc.active, Services: map[string]ServiceStatus{"web": {Kind: "public"}}}}
		r := f.req
		r.Action = tc.action
		r.Service = tc.selected
		r.Config = nil
		f.m.enrichDomainResponse(context.Background(), r, &resp)
		if calls.Load() != tc.probes {
			t.Fatalf("%+v probes %d", tc, calls.Load())
		}
		if tc.selected != "" {
			continue
		}
		ds := resp.Status.Services["web"].Domains
		if len(ds) != 2 || ds[0].Hostname != "new.example.com" || ds[0].Route != "pending" || ds[0].DNS != "unknown" || ds[1].Route != "configured" {
			t.Fatalf("domains %+v", ds)
		}
		if tc.probes > 0 && ds[1].HTTPS != "pending" {
			t.Fatalf("certificate pending %+v", ds[1])
		}
	}
}
func TestDomainHTTPSNeedsBothDNSAndCertificate(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(statusDomainApp("live.example.com"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ dns, cert, want string }{
		{"matches-host", "ready", "ready"}, {"pending", "ready", "pending"}, {"unverified", "ready", "pending"}, {"matches-host", "pending", "pending"},
	} {
		t.Run(tc.dns+tc.cert, func(t *testing.T) {
			f.m.DomainProbe = func(context.Context, string) DomainStatus { return DomainStatus{DNS: tc.dns, Certificate: tc.cert} }
			resp := Response{Status: &Status{Services: map[string]ServiceStatus{"web": {Kind: "public"}}}}
			r := f.req
			r.Action = "status"
			r.Config = nil
			f.m.enrichDomainResponse(context.Background(), r, &resp)
			if got := resp.Status.Services["web"].Domains[0].HTTPS; got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
