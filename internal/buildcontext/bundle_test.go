package buildcontext

import (
	"testing"

	"github.com/nchapman/soar/internal/config"
)

func TestNormalizedBuildUsesPortableConfigPaths(t *testing.T) {
	got := normalizedBuild(config.Build{Context: "./apps/web", Dockerfile: "docker/./Dockerfile"})
	if got.Context != "apps/web" || got.Dockerfile != "docker/Dockerfile" {
		t.Fatalf("platform-dependent bundle identity: %+v", got)
	}
}

func TestBundleDedupAndCorruption(t *testing.T) {
	a := &config.App{Services: map[string]config.Service{
		"web":     {Build: &config.Build{Context: "."}},
		"worker":  {Build: &config.Build{Context: "./", Dockerfile: "./Dockerfile"}},
		"backend": {Build: &config.Build{Context: ".", Dockerfile: "Other"}},
	}}
	calls := 0
	data, err := Bundle("soar.yaml", a, func(_ string, b config.Build) ([]byte, error) { calls++; return []byte(b.Dockerfile), nil })
	if err != nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	got, err := ReadBundle(data, a)
	if err != nil || len(got) != 2 {
		t.Fatalf("got=%v err=%v", got, err)
	}
	for _, bad := range [][]byte{data[:3], data[:len(data)-1], append(append([]byte{}, data...), 0)} {
		if _, err := ReadBundle(bad, a); err == nil {
			t.Fatal("accepted corrupt bundle")
		}
	}
	bad := append([]byte{}, data...)
	bad[len(bad)-1] ^= 1
	if _, err := ReadBundle(bad, a); err == nil {
		t.Fatal("accepted hash mismatch")
	}
	_, err = Bundle("", a, func(string, config.Build) ([]byte, error) { return make([]byte, MaxBytes/2), nil })
	if err == nil {
		t.Fatal("accepted oversized aggregate")
	}
}
