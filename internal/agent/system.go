package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/user"
	"strconv"

	"github.com/nchapman/vamos/internal/agentproto"
	"github.com/nchapman/vamos/internal/bridgeacl"
)

const (
	DefaultSocket         = "/run/vamos/agent.sock"
	DefaultQueueRoot      = "/var/lib/vamos/queue"
	DefaultPrincipals     = "/etc/vamos/principals.json"
	DefaultConfig         = "/etc/vamos/agent.json"
	DefaultAuthorizedKeys = "/etc/vamos/authorized_keys"
)

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
	principals, err := bridgeacl.KeyedPrincipals(DefaultPrincipals, DefaultAuthorizedKeys)
	if err != nil {
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
	return (&Daemon{
		SocketPath:         DefaultSocket,
		Queue:              queue,
		Principals:         principals,
		PrincipalsPath:     DefaultPrincipals,
		AuthorizedKeysPath: DefaultAuthorizedKeys,
		SocketGID:          gid,
	}).Serve(ctx)
}

func ValidateAuthorizedKeys(path string, principals map[string]agentproto.Principal) error {
	return bridgeacl.ValidateAuthorizedKeys(path, principals)
}

func ReadConfig(path string) (Config, error) {
	var config Config
	data, err := bridgeacl.ReadRegularFile(path, 1<<20, "agent configuration")
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

func ReadPrincipals(path string) (map[string]agentproto.Principal, error) {
	return bridgeacl.ReadPrincipals(path)
}
