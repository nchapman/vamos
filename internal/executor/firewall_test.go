package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/nchapman/vamos/internal/lifecycle"
)

func TestFirewallProtocolRefusals(t *testing.T) {
	cfg := &lifecycle.Config{Name: "home", Domain: "example.com"}
	for _, r := range []Request{
		{Operation: "firewall-enable", Config: cfg},
		{Operation: "firewall-confirm", Config: cfg, Confirm: true},
		{Operation: "firewall-disable", Config: cfg, Confirm: true, Token: "unexpected"},
		{Operation: "firewall-enable", Config: cfg, Confirm: true, Purge: true},
		{Operation: "firewall-enable", Config: cfg, Confirm: true, DeployKey: "key"},
		{Operation: "firewall-status", Config: cfg, Confirm: true},
		{Operation: "firewall-status", Config: cfg, Token: "token"},
		{Operation: "firewall-status"},
		{Operation: "inspect", Token: "token"},
		{Operation: "bootstrap", Config: cfg, Confirm: true, Token: "token"},
	} {
		t.Run(r.Operation, func(t *testing.T) {
			r.Version = Version
			data, _ := json.Marshal(r)
			var out bytes.Buffer
			code := serve(context.Background(), bytes.NewReader(data), &out, func(context.Context) ([]byte, error) { t.Fatal("invalid request probed host"); return nil, nil })
			response := readResponse(t, &out)
			if code != 2 || response.Error == nil || response.Error.Code != "invalid_request" {
				t.Fatalf("code=%d response=%+v", code, response)
			}
		})
	}
}
