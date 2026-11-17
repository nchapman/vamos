package ssh

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/firewall"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

func TestFirewallStatusPreservesDegradation(t *testing.T) {
	request := executor.Request{Version: executor.Version, Operation: "firewall-status", Config: &lifecycle.Config{Name: "home", Domain: "example.com"}}
	status, err := runFirewall(context.Background(), "root@home", request, []byte("binary"), func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{Stdout: []byte(`{"version":1,"firewall":{"degraded":true,"problems":["owned hook missing"]}}`)}, nil
	})
	if err != nil || status == nil || !status.Degraded || len(status.Problems) != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestFirewallStrictResponsesAndTransport(t *testing.T) {
	request := executor.Request{Version: executor.Version, Operation: "firewall-confirm", Confirm: true, Token: "token", Config: &lifecycle.Config{Name: "home", Domain: "example.com"}}
	good, _ := json.Marshal(executor.Response{Version: executor.Version, Firewall: &firewall.Status{Enabled: true, SSHPorts: []int{22}}})
	for _, data := range []string{string(good), `{"version":1}`, `{"version":2,"firewall":{"enabled":true}}`, `{"version":1,"firewall":{"enabled":true,"pending":true}}`, `{"version":1,"firewall":{"enabled":true},"error":{"code":"bad","message":"bad"}}`, string(good) + ` {}`, `{"version":1,"firewall":{"enabled":true,"unknown":true}}`} {
		status, err := runFirewall(context.Background(), "root@home", request, []byte("binary"), func(_ context.Context, name string, args []string, input []byte) (process.Result, error) {
			joined := strings.Join(args, " ")
			if name != "ssh" || !strings.Contains(joined, "ControlMaster=no") || !strings.Contains(joined, "ControlPath=none") || !strings.Contains(joined, "SSH_CONNECTION") || len(input) == 0 {
				t.Fatalf("transport %s %v", name, args)
			}
			return process.Result{Stdout: []byte(data)}, nil
		})
		if (err == nil) != (data == string(good)) {
			t.Fatalf("data=%s status=%+v err=%v", data, status, err)
		}
	}
}
