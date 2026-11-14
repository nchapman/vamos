package buildkit

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

func TestRootCredentialsAndHelpersKeepTheirEnvironmentWithoutEndpointOverrides(t *testing.T) {
	m, f := newEngine(t)
	config := filepath.Join(f.root, "operator-docker")
	if err := os.Mkdir(config, 0700); err != nil {
		t.Fatal(err)
	}
	// Synthetic basic credentials and a helper reference stay in the operator's
	// existing file; the build service must neither copy nor rewrite them.
	credentials := `{"auths":{"fixture.registry":{"auth":"Zml4dHVyZTpkdW1teQ=="}},"credHelpers":{"helper.registry":"soar-test"}}`
	if err := os.WriteFile(filepath.Join(config, "config.json"), []byte(credentials), 0600); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(f.root, "docker-credential-soar-test")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n[ \"$SOAR_FIXTURE_TOKEN\" = dummy ] && [ \"$1\" = get ]\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", f.root)
	t.Setenv("PATH", f.root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", config)
	t.Setenv("SOAR_FIXTURE_TOKEN", "dummy")
	for _, key := range []string{"DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "BUILDKIT_HOST", "BUILDX_BUILDER", "DOCKER_HOST", "BUILDX_CONFIG"} {
		t.Setenv(key, "foreign-selector")
	}
	m.Run = process.Run
	b := build{m: m, dir: filepath.Join(f.root, "var/lib/soar/buildkit")}
	script := `test -f "$DOCKER_CONFIG/config.json" && test -x "$HOME/docker-credential-soar-test" &&
 test "$DOCKER_HOST" = unix:///var/run/docker.sock &&
 test "$BUILDX_CONFIG" = "$HOME/var/lib/soar/buildkit/buildx" &&
 test "${DOCKER_CONTEXT+x}${DOCKER_TLS+x}${DOCKER_TLS_VERIFY+x}${DOCKER_CERT_PATH+x}${BUILDKIT_HOST+x}${BUILDX_BUILDER+x}" = "" &&
 docker-credential-soar-test get`
	if _, err := b.command(context.Background(), "sh", []string{"-c", script}, nil); err != nil {
		t.Fatal("credential/helper compatibility or endpoint constraints failed")
	}
	actual, err := os.ReadFile(filepath.Join(config, "config.json"))
	if err != nil || string(actual) != credentials {
		t.Fatal("operator credentials were changed")
	}
}
