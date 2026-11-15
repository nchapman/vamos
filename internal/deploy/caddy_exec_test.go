package deploy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/process"
)

func TestCaddyCLIRunsInsideManagedContainer(t *testing.T) {
	m, _ := setup(t)
	var name string
	var args []string
	m.Run = func(_ context.Context, n string, a []string, _ []byte) (process.Result, error) {
		name, args = n, a
		return process.Result{Stdout: []byte(`{}`)}, nil
	}
	if _, err := m.adapt(context.Background(), baseCaddy); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "vamos-caddy", "caddy", "adapt", "--config", "/etc/caddy/.vamos-adapt.Caddyfile", "--adapter", "caddyfile"}
	if name != "docker" || !reflect.DeepEqual(args, want) {
		t.Fatalf("ran %s %v, want docker %v", name, args, want)
	}
}

func TestCaddyCLIFailureKeepsCaddyDiagnostics(t *testing.T) {
	m, _ := setup(t)
	m.Run = func(_ context.Context, _ string, _ []string, _ []byte) (process.Result, error) {
		return process.Result{Stderr: []byte("Error: adapting config: unrecognized directive: bogus")}, errors.New("exit status 1")
	}
	_, err := m.caddy(context.Background(), "validate", "--config", "/etc/caddy/Caddyfile")
	if err == nil || !strings.Contains(err.Error(), "unrecognized directive") {
		t.Fatalf("diagnostic lost: %v", err)
	}
}
