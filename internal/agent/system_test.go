package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nchapman/vamos/internal/agentproto"
)

const testEd25519Key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIQqLpYe5SEsOj8XvbqPYYu76rg3HtBUOTcX7BBFWG4i deployer@example"

func writeKeys(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "authorized_keys")
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateAuthorizedKeysAcceptsLifecycleBridgeLine(t *testing.T) {
	principals := map[string]agentproto.Principal{
		"default-deployer": {Apps: []string{"*"}},
	}
	path := writeKeys(t, `restrict,command="/usr/local/lib/vamos/vamos-agent internal-agent bridge --principal default-deployer" `+testEd25519Key)
	if err := ValidateAuthorizedKeys(path, principals); err != nil {
		t.Fatalf("lifecycle-generated bridge line rejected: %v", err)
	}
}

func TestValidateAuthorizedKeysRejectsUnknownPrincipal(t *testing.T) {
	principals := map[string]agentproto.Principal{
		"default-deployer": {Apps: []string{"*"}},
	}
	path := writeKeys(t, `restrict,command="/usr/local/lib/vamos/vamos-agent internal-agent bridge --principal mallory" `+testEd25519Key)
	err := ValidateAuthorizedKeys(path, principals)
	if err == nil {
		t.Fatal("expected unknown principal to be rejected")
	}
	want := `authorized key line 1 names unknown principal "mallory"`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestValidateAuthorizedKeysRejectsUnrestrictedLine(t *testing.T) {
	principals := map[string]agentproto.Principal{
		"default-deployer": {Apps: []string{"*"}},
	}
	path := writeKeys(t, testEd25519Key)
	if err := ValidateAuthorizedKeys(path, principals); err == nil {
		t.Fatal("expected unrestricted key line to be rejected")
	}
	path = writeKeys(t, `command="/usr/local/lib/vamos/vamos-agent internal-agent bridge --principal default-deployer" `+testEd25519Key)
	if err := ValidateAuthorizedKeys(path, principals); err == nil {
		t.Fatal("expected key line without restrict option to be rejected")
	}
}

func TestValidateAuthorizedKeysAndConfigRejectSymlinks(t *testing.T) {
	dir := t.TempDir()
	principals := map[string]agentproto.Principal{
		"default-deployer": {Apps: []string{"*"}},
	}
	realKeys := writeKeys(t, `restrict,command="/usr/local/lib/vamos/executor internal-agent bridge --principal default-deployer" `+testEd25519Key)
	if err := ValidateAuthorizedKeys(realKeys, principals); err != nil {
		t.Fatalf("executor forced key rejected: %v", err)
	}
	linkKeys := filepath.Join(dir, "keys-link")
	if err := os.Symlink(realKeys, linkKeys); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAuthorizedKeys(linkKeys, principals); err == nil {
		t.Fatal("symlinked authorized_keys accepted")
	}

	cfgPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(cfgPath, []byte(`{"concurrency":{"max_queue_depth":32,"max_staged_bytes":536870912,"max_concurrent_builds":1,"max_concurrent_deploys":1}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(cfgPath); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	linkCfg := filepath.Join(dir, "cfg-link")
	if err := os.Symlink(cfgPath, linkCfg); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(linkCfg); err == nil {
		t.Fatal("symlinked agent.json accepted")
	}
}
