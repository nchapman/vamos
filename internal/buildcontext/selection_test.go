package buildcontext

import (
	"github.com/nchapman/vamos/internal/config"
	"testing"
)

func TestImageOnlyBundle(t *testing.T) {
	a := &config.App{Services: map[string]config.Service{"db": {Image: "postgres:17"}}}
	data, err := Bundle("missing.yaml", a, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ReadBundle(data, a)
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if string(data[4:]) != `{"version":1,"entries":[]}` {
		t.Fatalf("noncanonical manifest %s", data[4:])
	}
	for _, invalid := range []*config.App{nil, {}} {
		if _, err := Bundle("", invalid, nil); err == nil {
			t.Fatal("accepted empty app")
		}
	}
}

func TestSelectedBundleIgnoresUnusedContext(t *testing.T) {
	a := &config.App{Services: map[string]config.Service{"web": {Build: &config.Build{Context: "web"}}, "unused": {Build: &config.Build{Context: "missing"}}}}
	selected, err := Select(a, "web")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err = Bundle("vamos.yaml", selected, func(_ string, b config.Build) ([]byte, error) {
		calls++
		if b.Context != "web" {
			t.Fatalf("archived unused build: %+v", b)
		}
		return []byte("archive"), nil
	})
	if err != nil || calls != 1 || len(a.Services) != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if _, err := Select(a, "missing"); err == nil {
		t.Fatal("accepted unknown service")
	}
	if got, err := Select(a, ""); err != nil || got != a {
		t.Fatal("unscoped selection changed app")
	}
}
