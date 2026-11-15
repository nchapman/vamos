package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHJlcGxhY2VtZW50LXB1YmxpYy1rZXktYm9keS1vbmx5IGZvciB0ZXN0aW4g c i@host"

func TestValidateDeployKey(t *testing.T) {
	if err := ValidateDeployKey(testKey); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	for _, bad := range []string{
		"",
		"not a key",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFRVSU5JTkcgS0VZ a,b",
		"restrict,command=\"x\" " + testKey,
		testKey + "\n" + testKey,
		"ssh-ed25519 \"quoted\"",
		strings.Repeat("ssh-ed25519 AAAA", 400),
	} {
		if err := ValidateDeployKey(bad); err == nil {
			t.Fatalf("invalid key accepted: %q", bad)
		}
	}
}

func TestBootstrapProvisionsRestrictedBridgeKey(t *testing.T) {
	m, _ := newTestManager(t)
	m.DeployKey = testKey
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.path("/etc/soar/authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimRight(string(data), "\n")
	want := "restrict,command=\"" + AgentPath + " internal-agent bridge --principal default-deployer\" " + testKey
	if line != want || strings.Count(string(data), "\n") != 1 {
		t.Fatalf("bridge key line mismatch:\n got %q\nwant %q", line, want)
	}
	info, err := os.Lstat(m.path("/etc/soar/authorized_keys"))
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("bridge key permissions: %v %v", info, err)
	}
	principals, err := os.ReadFile(m.path("/etc/soar/principals.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(principals), `"apps":["*"]`) {
		t.Fatalf("default principal is not app-scoped for the bootstrap operator: %s", principals)
	}
}

func TestBootstrapWithoutDeployKeyLeavesBridgeClosed(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.path("/etc/soar/authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("bootstrap without a deploy key installed bridge access: %q", data)
	}
	if _, err := os.Stat(filepath.Join(m.path("/etc/soar"), "agent.json")); err != nil {
		t.Fatal(err)
	}
}
