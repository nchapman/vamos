package buildcontext

import (
	"path/filepath"
	"testing"

	"github.com/nchapman/vamos/internal/config"
)

func TestSecretFilesRequireSelectedIgnoreRule(t *testing.T) {
	root := t.TempDir()
	put(t, root, "Dockerfile", "FROM scratch")
	put(t, root, ".env", "TOKEN=synthetic-context-canary")
	put(t, root, ".dockerignore", ".env\n")
	capture := func() map[string]string {
		t.Helper()
		b, err := Archive(filepath.Join(root, "vamos.yaml"), config.Build{Context: ".", Dockerfile: "Dockerfile"})
		if err != nil {
			t.Fatal(err)
		}
		return unpack(t, b)
	}
	if _, ok := capture()[".env"]; ok {
		t.Fatal("explicitly ignored env file captured")
	}
	put(t, root, "Dockerfile.dockerignore", "# overrides the generic ignore file\n")
	if _, ok := capture()[".env"]; !ok {
		t.Fatal("test must expose selected-ignore precedence; no automatic credential detection")
	}
	put(t, root, "Dockerfile.dockerignore", ".env\n")
	if _, ok := capture()[".env"]; ok {
		t.Fatal("Dockerfile-specific env exclusion ignored")
	}
}
