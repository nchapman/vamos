package ssh

import (
	"context"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
)

func TestAppsReadOnlyResponseAcceptance(t *testing.T) {
	request := deploy.Request{Version: 1, Action: "apps", Host: lifecycle.Config{Name: "home", Domain: "example.com"}}
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"empty", `{"version":1,"apps":{"entries":[]}}`, true},
		{"entries", `{"version":1,"apps":{"entries":[{"app":"notes","phase":"staged","services":[],"service_count":0,"retained_secrets":true,"volume_records":0,"retained_volumes":false}]}}`, true},
		{"missing", `{"version":1}`, false},
		{"wrong-kind", `{"version":1,"status":{"app":"notes"}}`, false},
		{"remote-error", `{"version":1,"error":"invalid app metadata"}`, false},
		{"trailing", `{"version":1,"apps":{"entries":[]}} {}`, false},
		{"version", `{"version":2,"apps":{"entries":[]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := runApp(context.Background(), "root@home", request, []byte("binary"), nil, func(_ context.Context, program string, args []string, input []byte) (process.Result, error) {
				command := args[len(args)-1]
				if program != "ssh" || !strings.Contains(command, "/run/vamos-read.") {
					t.Fatalf("not read-only transport: %s %v", program, args)
				}
				return process.Result{Stdout: []byte(tc.data)}, nil
			})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t response=%+v err=%v", tc.valid, response, err)
			}
			if tc.valid && response.Apps == nil {
				t.Fatal("missing inventory")
			}
		})
	}
}
