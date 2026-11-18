package bridgeacl

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const publicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIQqLpYe5SEsOj8XvbqPYYu76rg3HtBUOTcX7BBFWG4i"
const manual = `[{"id":"default-deployer","role":"deployer","apps":["*"]},{"id":"admin","role":"admin"}]`

func fixture(t *testing.T, keys string) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string]string{"principals.json": manual, "authorized_keys": keys} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGrantRevokeReplay(t *testing.T) {
	const retained = "# bootstrap keys\r\n\n" + `restrict,command="/usr/local/lib/vamos/executor internal-agent bridge --principal default-deployer" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f bootstrap` + "\r\n"
	dir := fixture(t, retained)
	grant := Request{Action: "grant", Name: "ci", App: "api", PublicKey: publicKey}
	apply := func(r Request) []Entry {
		t.Helper()
		entries, err := Apply(dir, r)
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}
	pp, kp := filepath.Join(dir, "principals.json"), filepath.Join(dir, "authorized_keys")
	generationID := func() string {
		t.Helper()
		ps, err := ReadPrincipals(pp)
		if err != nil {
			t.Fatal(err)
		}
		if ps["admin"].Role != "admin" || ps["default-deployer"].Apps[0] != "*" {
			t.Fatal("manual principals changed")
		}
		for id, p := range ps {
			if strings.HasPrefix(id, prefix) {
				if p.Role != "deployer" || len(p.Apps) != 1 || p.Apps[0] != "api" {
					t.Fatalf("unexpected scope: %+v", p)
				}
				return id
			}
		}
		return ""
	}
	apply(grant)
	first := generationID()
	if first == "" {
		t.Fatal("missing principal")
	}
	grant.PublicKey += " changed-comment"
	apply(grant)
	if generationID() != first {
		t.Fatal("grant replay rotated principal")
	}
	list := apply(Request{Action: "list"})
	if len(list) != 1 || list[0] != (Entry{"ci", "api", "SHA256:f2nBCm81N7U3QBdJBoxMM8DXIMLVgejzZ/91ffhd68w"}) {
		t.Fatalf("entries: %+v", list)
	}
	// Simulate the durable intermediate state of either operation.
	if err := os.WriteFile(kp, []byte(retained), 0644); err != nil {
		t.Fatal(err)
	}
	active, err := KeyedPrincipals(pp, kp)
	if err != nil || len(active) != 1 || active["default-deployer"].ID == "" {
		t.Fatalf("intermediate active set: %+v, %v", active, err)
	}
	apply(grant)
	second := generationID()
	if second == first {
		t.Fatal("orphaned generation reused")
	}
	revoke := Request{Action: "revoke", Name: "ci"}
	apply(revoke)
	apply(revoke)
	if generationID() != "" {
		t.Fatal("revoke left principal")
	}
	b, err := os.ReadFile(kp)
	if err != nil || string(b) != retained {
		t.Fatalf("unrelated key lines changed: %q, %v", b, err)
	}
	apply(grant)
	if generationID() == second {
		t.Fatal("revoked generation reused")
	}
}

func TestManualCollisions(t *testing.T) {
	line := `restrict,command="/usr/local/lib/vamos/executor internal-agent bridge --principal default-deployer" ` + publicKey + " manual\n"
	dir := fixture(t, line)
	for _, req := range []Request{
		{Action: "grant", Name: "ci", App: "api", PublicKey: publicKey + " another-comment"},
		{Action: "revoke", Name: "admin"},
	} {
		if _, err := Apply(dir, req); err == nil {
			t.Fatalf("accepted collision: %+v", req)
		}
	}
	entries, err := Apply(dir, Request{Action: "list"})
	if err != nil || len(entries) != 0 {
		t.Fatalf("list = %+v, %v", entries, err)
	}
}

func TestValidation(t *testing.T) {
	for _, req := range []Request{
		{Action: "unknown"}, {Action: "list", Name: "foo"}, {Action: "revoke", Name: "../x"},
		{Action: "grant", Name: "default-deployer", App: "api", PublicKey: publicKey},
		{Action: "grant", Name: "ci", App: "*", PublicKey: publicKey},
		{Action: "grant", Name: "ci", App: "api", PublicKey: strings.Replace(publicKey, "ssh-ed25519", "ssh-rsa", 1)},
		{Action: "grant", Name: "ci", App: "api", PublicKey: publicKey + "AAAA"},
		{Action: "grant", Name: "ci", App: "api", PublicKey: publicKey + "\n"},
	} {
		if req.Validate() == nil {
			t.Errorf("accepted %+v", req)
		}
	}
}

func TestOwnerBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix installation")
	}
	dir := fixture(t, "")
	path := filepath.Join(dir, "principals.json")
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(dir, Request{Action: "list"}); err == nil {
		t.Fatal("accepted writable principals")
	}
	if os.Geteuid() == 0 {
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, 65534, -1); err != nil {
			t.Fatal(err)
		}
		if _, err := Apply(dir, Request{Action: "list"}); err == nil {
			t.Fatal("accepted foreign owner")
		}
	}
}
