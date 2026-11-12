package buildcontext

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nchapman/soar/internal/config"
)

// Also run as a compiled test binary under a low descriptor limit: each add()
// invocation must close its own file before the next walk iteration.
func TestArchiveManySmallFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%04d", i)), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Archive(filepath.Join(dir, "soar.yaml"), config.Build{Context: ".", Dockerfile: "Dockerfile"}); err != nil {
		t.Fatal(err)
	}
}
