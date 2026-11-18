package executor

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestPrincipalRequestIsolation(t *testing.T) {
	for _, data := range []string{
		`{"version":1,"operation":"inspect","principal":null}`,
		`{"version":1,"operation":"firewall-status","principal":{}}`,
		`{"version":1,"operation":"principal","config":{"name":"home","domain":"example.com"},"principal":{"action":"revoke","action":"list"}}`,
		`{"version":1,"operation":"principal","config":{"name":"home","domain":"example.com"},"principal":{"action":"list","name":null}}`,
		`{"version":1,"operation":"principal","config":{"name":"home","domain":"example.com"},"principal":{"action":"list"},"token":""}`,
		`{"version":1,"operation":"principal","config":{"name":"home","domain":"example.com"},"principal":{"action":"list","unknown":false}}`,
		`{"version":1,"operation":"principal","config":{"name":"home","domain":"example.com"},"principal":{"action":"grant"}}`,
	} {
		var out bytes.Buffer
		code := serve(context.Background(), strings.NewReader(data), &out, func(context.Context) ([]byte, error) { t.Fatal("unexpected probe"); return nil, nil })
		if code != 2 {
			t.Fatalf("request %s returned %d", data, code)
		}
	}
}
