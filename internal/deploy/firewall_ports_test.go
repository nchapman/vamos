package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/config"
)

func TestManagedFirewallReservesSSHPort(t *testing.T) {
	m, _ := setup(t)
	j := routeTestJournal("notes")
	j.ServiceStates = nil // This test exercises desired bindings, not runtime claims.
	path := m.path("/var/lib/vamos/firewall.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	receipt := `{"version":1,"host_id":"` + j.HostID + `","token":"` + strings.Repeat("a", 64) + `","connection":"203.0.113.1 50000 192.0.2.1 2222","deadline":"2030-01-01T00:00:00Z","plan":{"ssh_ports":[2222],"bridges":["br-+","vamos-build0"]}}`
	if err := os.WriteFile(path, []byte(receipt), 0600); err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"tcp", "udp"} {
		j.Config.Services["test"] = config.Service{Image: "busybox", Ports: []string{"2221-2223:8000-8002/" + protocol}}
		err := m.reserveManagedPorts([]*journal{j}, j.HostID)
		if protocol == "tcp" && (err == nil || !strings.Contains(err.Error(), "management SSH")) {
			t.Fatalf("TCP conflict: %v", err)
		}
		if protocol == "udp" && err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	j.Config.Services["test"] = config.Service{Image: "busybox", Ports: []string{"2222:8000"}}
	if err := m.reserveManagedPorts([]*journal{j}, j.HostID); err != nil {
		t.Fatalf("user-managed policy: %v", err)
	}
}
