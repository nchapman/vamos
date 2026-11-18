package ssh

import (
	"context"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/bridgeacl"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

func TestPrincipalAdminTransport(t *testing.T) {
	cfg := lifecycle.Config{Name: "home", Domain: "example.com"}
	good := `{"version":1,"entries":[]}`
	entry := `{"name":"ci","app":"web","fingerprint":"SHA256:` + strings.Repeat("A", 43) + `"}`
	list := `{"version":1,"entries":[` + entry + `]}`
	for _, data := range []string{good, list, `{"version":1,"entries":[` + entry + `,` + entry + `]}`, `{"version":1,"version":1,"entries":[]}`, `{"version":1,"Entries":[]}`, `{"version":1}`, `{"version":2,"entries":[]}`, `{"version":1,"entries":[],"result":null}`, `{"version":1,"entries":[],"error":{"code":"x","message":"x"}}`, good + ` {}`, `{"version":1,"entries":[{"name":"ci","app":"web","fingerprint":"bad"}]}`} {
		_, err := runPrincipal(context.Background(), "operator@home", cfg, bridgeacl.Request{Action: "list"}, []byte("bundle"), func(_ context.Context, exe string, args []string, input []byte) (process.Result, error) {
			s := strings.Join(args, " ")
			if exe != "ssh" || !strings.Contains(s, "sudo -n") || !strings.Contains(s, "ControlMaster=no") || !strings.Contains(string(input), `"operation":"principal"`) {
				t.Fatal("missing admin staging")
			}
			return process.Result{Stdout: []byte(data)}, nil
		})
		if (err == nil) != (data == good || data == list) {
			t.Fatalf("reply %s: %v", data, err)
		}
	}
}
