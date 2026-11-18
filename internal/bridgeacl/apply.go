package bridgeacl

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nchapman/vamos/internal/agentproto"
	"github.com/nchapman/vamos/internal/appstate"
)

// The entire prefix is reserved. Never adopt a malformed or ambiguous entry.
const prefix = "vamos-cli/"

var generation = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Request struct {
	Action    string `json:"action"`
	Name      string `json:"name,omitempty"`
	App       string `json:"app,omitempty"`
	PublicKey string `json:"public_key,omitempty"`
}

type Entry struct {
	Name        string `json:"name"`
	App         string `json:"app"`
	Fingerprint string `json:"fingerprint"`
}

func validName(name string) bool { return appstate.ValidName(name) && name != "default-deployer" }

func (r Request) Validate() error {
	switch r.Action {
	case "list":
		if r.Name == "" && r.App == "" && r.PublicKey == "" {
			return nil
		}
	case "revoke":
		if validName(r.Name) && r.App == "" && r.PublicKey == "" {
			return nil
		}
	case "grant":
		if validName(r.Name) && appstate.ValidName(r.App) {
			_, err := fingerprint(r.PublicKey)
			return err
		}
	}
	return errors.New("invalid principal request action, name, app, or fields")
}

type grant struct {
	principal agentproto.Principal
	key       *keyLine // nil after an interrupted mutation
}

func managedGrants(ps map[string]agentproto.Principal, keys []keyLine) (map[string]*grant, error) {
	grants := make(map[string]*grant)
	for id, p := range ps {
		tail, ok := strings.CutPrefix(id, prefix)
		if !ok {
			continue
		}
		name, gen, ok := strings.Cut(tail, "/")
		if !ok || !validName(name) || !generation.MatchString(gen) || grants[name] != nil {
			return nil, fmt.Errorf("invalid reserved principal ID %q", id)
		}
		if p.Role != agentproto.RoleDeployer || len(p.Actions) != 0 || len(p.Apps) != 1 || !appstate.ValidName(p.Apps[0]) {
			return nil, fmt.Errorf("invalid reserved principal scope for %q", id)
		}
		if _, collision := ps[name]; collision {
			return nil, fmt.Errorf("ambiguous principal name %q", name)
		}
		grants[name] = &grant{principal: p}
	}
	for i := range keys {
		tail, managed := strings.CutPrefix(keys[i].id, prefix)
		if managed {
			name, _, _ := strings.Cut(tail, "/")
			g := grants[name] // readKeys already verified the principal ID
			if g.key != nil {
				return nil, errors.New("multiple keys for managed principal")
			}
			g.key = &keys[i]
		}
	}
	return grants, nil
}

// Apply requires the caller to hold the lifecycle lock. The only intermediate
// state is a managed principal with no key. Never reuse such a principal: it
// might belong to a partially revoked generation with an open SSH session.
func Apply(dir string, req Request) ([]Entry, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	pp, kp := filepath.Join(dir, "principals.json"), filepath.Join(dir, "authorized_keys")
	ps, err := ReadPrincipals(pp)
	if err != nil {
		return nil, err
	}
	lines, keys, err := readKeys(kp, ps)
	if err != nil {
		return nil, err
	}
	grants, err := managedGrants(ps, keys)
	if err != nil {
		return nil, err
	}
	if req.Action == "list" {
		entries := []Entry{}
		for name, g := range grants {
			if g.key != nil {
				entries = append(entries, Entry{name, g.principal.Apps[0], g.key.fp})
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		return entries, nil
	}
	if _, collision := ps[req.Name]; collision {
		return nil, errors.New("name belongs to an unmanaged principal")
	}
	g := grants[req.Name]
	if req.Action == "revoke" {
		if g == nil {
			return nil, nil
		}
		if g.key != nil {
			lines[g.key.index] = ""
		}
		delete(ps, g.principal.ID)
	} else {
		// Keep even a maximum-length inventory within the admin transport's 64 KiB response.
		if g == nil && len(grants) >= 256 {
			return nil, errors.New("managed deploy key limit is 256 per server")
		}
		fp, _ := fingerprint(req.PublicKey) // validated above
		for _, k := range keys {
			if k.fp == fp && (g == nil || k.id != g.principal.ID) {
				return nil, errors.New("key already authorized")
			}
		}
		if g != nil {
			if g.key != nil {
				if g.key.fp == fp && g.principal.Apps[0] == req.App {
					return nil, nil
				}
				return nil, errors.New("name already granted")
			}
			delete(ps, g.principal.ID)
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		id := prefix + req.Name + "/" + hex.EncodeToString(nonce[:])
		ps[id] = agentproto.Principal{ID: id, Role: agentproto.RoleDeployer, Apps: []string{req.App}}
		if last := lines[len(lines)-1]; last != "" && !strings.HasSuffix(last, "\n") {
			lines = append(lines, "\n")
		}
		lines = append(lines, `restrict,command="/usr/local/lib/vamos/vamos-agent internal-agent bridge --principal `+id+`" `+req.PublicKey+"\n")
	}
	ordered := make([]agentproto.Principal, 0, len(ps))
	for _, p := range ps {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	data, err := json.Marshal(ordered)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	keyData := []byte(strings.Join(lines, ""))
	if len(data) > maxFile || len(keyData) > maxFile {
		return nil, errors.New("ACL exceeds size limit")
	}
	if req.Action == "grant" {
		if err := replace(pp, data, 0600); err != nil {
			return nil, err
		}
		return nil, replace(kp, keyData, 0644)
	}
	if g.key != nil {
		if err := replace(kp, keyData, 0644); err != nil {
			return nil, err
		}
	}
	return nil, replace(pp, data, 0600)
}

// Sync both the contents and the rename before advancing to the next file.
func replace(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".acl-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
