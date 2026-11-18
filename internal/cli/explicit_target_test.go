package cli

import (
	"bytes"
	"flag"
	"io"
	"strings"
	"testing"
)

func TestExplicitTargetRejected(t *testing.T) {
	// An unusable config root makes accidental registry access fail, without a real target.
	t.Setenv("HOME", "/dev/null")
	t.Setenv("XDG_CONFIG_HOME", "/dev/null")
	for _, tc := range []struct {
		args     []string
		selector string
	}{
		{[]string{"server", "status", ""}, "NAME"},
		{[]string{"server", "upgrade", "--yes", "prod", ""}, "TARGET"},
		{[]string{"server", "firewall", "disable", "--yes", ""}, "NAME"},
		{[]string{"server", "principal", "list", "--server="}, "server"},
		{[]string{"destroy", "--yes", "--app="}, "app"},
		{[]string{"apps", "--server", ""}, "server"},
		{[]string{"service", "bind", "web", "--service=api"}, "service"},
		{[]string{"service", "remove", "web", "--service="}, "service"},
		{[]string{"service", "remove", "--service=web", "-service", "web"}, "service"},
		{[]string{"deploy", "--server=a", "--server=b"}, "server"},
		{[]string{"deploy", "--app=a", "--app=b"}, "app must not be repeated"},
		{[]string{"service", "remove", "web", "--server="}, "server must not be empty"},
		{[]string{"service", "bind", "--service=web", "--app="}, "app must not be empty"},
		{[]string{"server", "principal", "grant", "--key=a", "--key=b"}, "key must not be repeated"},
		{[]string{"backup", "repository", "add", "--bucket=a", "--bucket=b"}, "bucket must not be repeated"},
		{[]string{"server", "connect", "--domain=a.example", "--domain=b.example"}, "domain must not be repeated"},
		{[]string{"deploy", "-f", "one", "-f", "two"}, "-f"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var diag bytes.Buffer
			if code := Run(tc.args, io.Discard, &diag, "test"); code != 2 || !strings.Contains(diag.String(), tc.selector) {
				t.Fatalf("code=%d: %s", code, &diag)
			}
		})
	}
}

func TestSelectorParsingRespectsArgumentBoundaries(t *testing.T) {
	f := flag.NewFlagSet("test", flag.ContinueOnError)
	f.String("f", "", "config")
	server := f.String("server", "", "server")
	args := []string{"-f", "--server=not-a-selector", "--server", "prod", "--", "cmd", "--server="}
	if err := parseFlags(f, args, io.Discard, io.Discard); err != nil || *server != "prod" || f.NArg() != 2 {
		t.Fatalf("err=%v server=%q remaining=%q", err, *server, f.Args())
	}
}
