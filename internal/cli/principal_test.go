package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/bridgeacl"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/registry"
	"github.com/nchapman/vamos/internal/ssh"
)

func TestPrincipalUsageBeforeContact(t *testing.T) {
	for _, args := range [][]string{{"list", "--yes"}, {"list", "owner"}, {"grant", "ci"}, {"revoke", "ci"}, {"revoke", "--yes", "--app", "bill", "ci"}, {"grant", "--yes", "--app", "ledger", "ci"}, {"revoke", "--yes"}, {"list", "--timeout=0"}} {
		if code := serverPrincipal(context.Background(), args, io.Discard, io.Discard, lifecycleDeps{}); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestPrincipalCommandsUseAdminTarget(t *testing.T) {
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"
	path := filepath.Join(t.TempDir(), "ci.pub")
	if err := os.WriteFile(path, []byte(key+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := lifecycle.Config{Name: "prod", Domain: "apps.example.com"}
	store := registry.New(t.TempDir())
	target := "admin@server"
	if err := store.Put(registry.Server{Name: cfg.Name, Target: target, Domain: cfg.Domain}); err != nil {
		t.Fatal(err)
	}
	binary := []byte("host-bundle")
	for _, tc := range []struct {
		args     []string
		request  bridgeacl.Request
		contains string
	}{
		{[]string{"grant", "--server", "prod", "--yes", "--app", "ledger", "--key", path, "ci"}, bridgeacl.Request{Action: "grant", Name: "ci", App: "ledger", PublicKey: key}, "granted for app ledger"},
		{[]string{"list", "--json"}, bridgeacl.Request{Action: "list"}, `"name":"ci"`},
		{[]string{"revoke", "--yes", "ci"}, bridgeacl.Request{Action: "revoke", Name: "ci"}, "revoked"},
	} {
		t.Run(tc.args[0], func(t *testing.T) {
			calls := 0
			deps := lifecycleDeps{Registry: func() (*registry.Store, error) { return store, nil }, Status: func(context.Context, string) (*ssh.Status, error) {
				return &ssh.Status{State: &lifecycle.State{Config: cfg, Phase: "ready", ExecutorSHA: fmt.Sprintf("%x", sha256.Sum256(binary))}}, nil
			}, Inspect: func(context.Context, string) (*host.Report, error) {
				return &host.Report{Root: true, Supported: true, Architecture: "arm64"}, nil
			}, Artifact: func(string, string) ([]byte, error) { return binary, nil }, Principal: func(_ context.Context, gotTarget string, gotCfg lifecycle.Config, req bridgeacl.Request, b []byte) ([]bridgeacl.Entry, error) {
				calls++
				if gotTarget != target || gotCfg != cfg || tc.request != req || !bytes.Equal(binary, b) {
					t.Fatalf("unexpected request %s %v %+v", gotTarget, gotCfg, req)
				}
				return []bridgeacl.Entry{{Name: "ci", App: "ledger"}}, nil
			}}
			var out, diag bytes.Buffer
			if code := serverPrincipal(context.Background(), tc.args, &out, &diag, deps); code != 0 || calls != 1 || !strings.Contains(out.String(), tc.contains) || strings.Contains(diag.String()+out.String(), key) {
				t.Fatalf("code=%d calls=%d stdout=%q stderr=%q", code, calls, out.String(), diag.String())
			}
		})
	}
}
