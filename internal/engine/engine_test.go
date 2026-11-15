package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/vamos/internal/config"
)

func TestValidateBridgeNetwork(t *testing.T) {
	v := &Inspection{
		Driver: "bridge",
		Labels: map[string]string{"vamos.host": "h1", "vamos.app": "app"},
		Options: map[string]string{
			"com.docker.network.bridge.enable_icc": "true",
		},
	}
	if err := ValidateBridgeNetwork(v, "h1", "app"); err != nil {
		t.Fatalf("valid network rejected: %v", err)
	}
	bad := *v
	bad.Internal = true
	if err := ValidateBridgeNetwork(&bad, "h1", "app"); err == nil {
		t.Fatal("internal network accepted")
	}
	badOpt := *v
	badOpt.Options = map[string]string{"com.docker.network.bridge.enable_icc": "false"}
	if err := ValidateBridgeNetwork(&badOpt, "h1", "app"); err == nil {
		t.Fatal("non-default network option accepted")
	}
}

func TestRunWithEnvFileCleansUpAndHidesSecretsFromArgv(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var seenArgs []string
	var seenFile string
	c := &Client{
		RuntimeDir: dir,
		Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			seenArgs = append([]string(nil), args...)
			idx := slices.Index(args, "--env-file")
			if idx < 0 || idx+1 >= len(args) {
				t.Fatalf("missing --env-file in %q", args)
			}
			b, err := os.ReadFile(args[idx+1])
			if err != nil {
				t.Fatal(err)
			}
			seenFile = string(b)
			return nil, nil
		},
	}
	err = c.RunWithEnvFile(context.Background(), "img:1", []string{"run", "--detach", "img:1", "serve"}, map[string]string{"API_KEY": "secret-val"})
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range seenArgs {
		if strings.Contains(arg, "secret-val") {
			t.Fatalf("secret leaked into argv: %q", seenArgs)
		}
	}
	if seenFile != "API_KEY=secret-val\n" {
		t.Fatalf("unexpected env file %q", seenFile)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temp env file left behind: entries=%d err=%v", len(entries), err)
	}
}

func TestWaitHTTPReady(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/healthz" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if calls < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	c := &Client{HTTP: srv.Client()}
	err := c.WaitHTTPReady(context.Background(), 2*time.Second, "/healthz", func(context.Context) (string, error) {
		return port, nil
	})
	if err != nil || calls < 2 {
		t.Fatalf("WaitHTTPReady failed: calls=%d err=%v", calls, err)
	}

	nilClient := &Client{}
	if err := nilClient.WaitHTTPReady(context.Background(), time.Second, "/healthz", func(context.Context) (string, error) {
		return "8080", nil
	}); err == nil {
		t.Fatal("expected error for nil HTTP client")
	}
	if err := c.WaitHTTPReady(context.Background(), time.Second, "/bad path", func(context.Context) (string, error) {
		return "not-a-port", nil
	}); err == nil {
		t.Fatal("expected error for invalid readiness URL")
	}
}

func TestResourceChecks(t *testing.T) {
	cpus := 1.0
	mem := int64(256)
	res := &config.Resources{CPUs: &cpus, MemoryMB: &mem}
	args := ResourceArgs(res)
	if !slices.Equal(args, []string{"--cpus", "1", "--memory", "268435456", "--memory-swap", "268435456"}) {
		t.Fatalf("unexpected args %q", args)
	}

	c := &Client{
		Run: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return json.Marshal(map[string]any{
				"MemoryLimit": true, "SwapLimit": false, "NCPU": 2,
				"CpuCfsQuota": true, "CpuCfsPeriod": true,
			})
		},
	}
	if err := c.RequireResourceSupport(context.Background(), res); err == nil || !strings.Contains(err.Error(), "SwapLimit") {
		t.Fatalf("expected SwapLimit refusal, got %v", err)
	}
}

func TestSafePathRejectsSymlink(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		if runtime.GOOS == "windows" || errors.Is(err, errors.ErrUnsupported) {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := SafePath(filepath.Join(link, "child")); err == nil {
		t.Fatal("symlink path accepted")
	}
}
