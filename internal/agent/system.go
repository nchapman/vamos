package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"

	"github.com/nchapman/vamos/internal/agentproto"
)

const (
	DefaultSocket         = "/run/vamos/agent.sock"
	DefaultQueueRoot      = "/var/lib/vamos/queue"
	DefaultPrincipals     = "/etc/vamos/principals.json"
	DefaultConfig         = "/etc/vamos/agent.json"
	DefaultAuthorizedKeys = "/etc/vamos/authorized_keys"
)

var forcedKeyRE = regexp.MustCompile(`^restrict,command="(?:/usr/local/lib/vamos/vamos-agent|/usr/local/lib/vamos/executor) internal-agent bridge --principal ([A-Za-z0-9][A-Za-z0-9._:@/-]{0,127})" ssh-(?:ed25519|rsa|ecdsa-sha2-nistp(?:256|384|521)) [A-Za-z0-9+/=]{68,}(?: [ -~]{0,511})?$`)

type Config struct {
	Concurrency agentproto.ConcurrencyPolicy `json:"concurrency"`
}

// ServeSystem starts the installed agent with the fixed, root-owned paths
// written by lifecycle.Manager. It is intentionally not configurable through
// SSH/client input.
func ServeSystem(ctx context.Context) error {
	config, err := ReadConfig(DefaultConfig)
	if err != nil {
		return err
	}
	principals, err := ReadPrincipals(DefaultPrincipals)
	if err != nil {
		return err
	}
	if err := ValidateAuthorizedKeys(DefaultAuthorizedKeys, principals); err != nil {
		return err
	}
	queue, err := NewQueue(DefaultQueueRoot, config.Concurrency)
	if err != nil {
		return err
	}
	group, err := user.LookupGroup("vamos")
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return err
	}
	return (&Daemon{SocketPath: DefaultSocket, Queue: queue, Principals: principals, SocketGID: gid}).Serve(ctx)
}

// ValidateAuthorizedKeys permits only restricted forced bridge commands whose
// principal IDs exist in the root-owned ACL. This makes key-to-principal
// attribution an sshd-enforced boundary rather than client-provided input.
func ValidateAuthorizedKeys(path string, principals map[string]agentproto.Principal) error {
	data, err := readRegularFile(path, 1<<20, "authorized_keys")
	if err != nil {
		return err
	}
	for lineNo, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		match := forcedKeyRE.FindStringSubmatch(line)
		if match == nil {
			return fmt.Errorf("authorized key line %d is not a restricted Vamos bridge key", lineNo+1)
		}
		if _, ok := principals[match[1]]; !ok {
			return fmt.Errorf("authorized key line %d names unknown principal %q", lineNo+1, match[1])
		}
	}
	return nil
}

func ReadConfig(path string) (Config, error) {
	var config Config
	data, err := readRegularFile(path, 1<<20, "agent configuration")
	if err != nil {
		return config, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&config); err != nil {
		return config, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return config, errors.New("agent configuration must contain exactly one JSON document")
	}
	if err := config.Concurrency.Validate(); err != nil {
		return config, err
	}
	return config, nil
}

// ReadPrincipals strictly loads the root-owned ACL file. Empty ACLs are valid:
// they make the bridge fail closed until an administrator provisions a scoped
// deploy key and matching principal.
func ReadPrincipals(path string) (map[string]agentproto.Principal, error) {
	data, err := readRegularFile(path, 1<<20, "principals file")
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var principals []agentproto.Principal
	if err := dec.Decode(&principals); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("principals file must contain exactly one JSON document")
	}
	result := make(map[string]agentproto.Principal, len(principals))
	for _, principal := range principals {
		if err := principal.Validate(); err != nil {
			return nil, err
		}
		if _, exists := result[principal.ID]; exists {
			return nil, errors.New("duplicate principal ID")
		}
		result[principal.ID] = principal
	}
	return result, nil
}

func readRegularFile(path string, maxBytes int64, label string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() > maxBytes {
		return nil, fmt.Errorf("%s must be a regular file no larger than %d bytes", label, maxBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Size() > maxBytes {
		return nil, fmt.Errorf("%s changed or exceeded size limit while reading", label)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s must be a regular file no larger than %d bytes", label, maxBytes)
	}
	return data, nil
}
