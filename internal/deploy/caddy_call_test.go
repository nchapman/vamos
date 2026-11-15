package deploy

import (
	"strings"

	"github.com/nchapman/vamos/internal/lifecycle"
)

// caddyCall recognizes the Caddy CLI run inside the managed ingress container
// and returns its arguments with /etc/caddy paths mapped onto the test root.
func caddyCall(m *Manager, name string, args []string) ([]string, bool) {
	if name != "docker" || len(args) < 4 || args[0] != "exec" || args[1] != lifecycle.CaddyContainer || args[2] != "caddy" {
		return nil, false
	}
	out := append([]string(nil), args[3:]...)
	for i, arg := range out {
		if strings.HasPrefix(arg, "/etc/caddy/") {
			out[i] = m.path(arg)
		}
	}
	return out, true
}
