package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"strconv"

	"github.com/nchapman/soar/internal/agentproto"
)

const (
	DefaultSocket     = "/run/soar/agent.sock"
	DefaultQueueRoot  = "/var/lib/soar/queue"
	DefaultPrincipals = "/etc/soar/principals.json"
)

// ServeSystem starts the installed agent with the fixed, root-owned paths
// written by lifecycle.Manager. It is intentionally not configurable through
// SSH/client input.
func ServeSystem(ctx context.Context) error {
	principals, err := ReadPrincipals(DefaultPrincipals)
	if err != nil {
		return err
	}
	queue, err := NewQueue(DefaultQueueRoot, agentproto.DefaultConcurrencyPolicy())
	if err != nil {
		return err
	}
	group, err := user.LookupGroup("soar")
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return err
	}
	return (&Daemon{SocketPath: DefaultSocket, Queue: queue, Principals: principals, SocketGID: gid}).Serve(ctx)
}

// ReadPrincipals strictly loads the root-owned ACL file. Empty ACLs are valid:
// they make the bridge fail closed until an administrator provisions a scoped
// deploy key and matching principal.
func ReadPrincipals(path string) (map[string]agentproto.Principal, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<20 {
		return nil, errors.New("principals file must be a regular file no larger than 1 MiB")
	}
	data, err := os.ReadFile(path)
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
