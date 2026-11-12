package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDockerInspectionAccommodatesBoundedSecretEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux executor command boundary")
	}
	dir := t.TempDir()
	v := inspection{}
	for i := 0; i < 64; i++ {
		v.Config.Env = append(v.Config.Env, "KEY="+strings.Repeat("\x01", 8192))
	}
	b, err := json.Marshal([]inspection{v})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "inspection.json")
	if err = os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexec /bin/cat \"$SOAR_TEST_INSPECTION\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SOAR_TEST_INSPECTION", file)
	got, err := NewManager().inspect(context.Background(), "container", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Config.Env) != 64 {
		t.Fatal("inspection truncated")
	}
}
