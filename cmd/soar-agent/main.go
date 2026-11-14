// soar-agent is the root-owned coordinator installed on a managed host.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"

	"github.com/nchapman/soar/internal/agent"
	"github.com/nchapman/soar/internal/agentproto"
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: soar-agent serve|bridge")
		return 2
	}
	switch args[0] {
	case "serve":
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		flags.SetOutput(stderr)
		socket := flags.String("socket", "/run/soar/agent.sock", "agent Unix socket")
		queueRoot := flags.String("queue", "/var/lib/soar/queue", "durable queue directory")
		principalsPath := flags.String("principals", "/etc/soar/principals.json", "principal ACL file")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return 2
		}
		principals, err := readPrincipals(*principalsPath)
		if err != nil {
			fmt.Fprintf(stderr, "agent ACL configuration: %v\n", err)
			return 1
		}
		queue, err := agent.NewQueue(*queueRoot, agentproto.DefaultConcurrencyPolicy())
		if err != nil {
			fmt.Fprintf(stderr, "agent queue: %v\n", err)
			return 1
		}
		group, err := user.LookupGroup("soar")
		if err != nil {
			fmt.Fprintf(stderr, "agent group: %v\n", err)
			return 1
		}
		gid, err := strconv.Atoi(group.Gid)
		if err != nil {
			fmt.Fprintf(stderr, "agent group: %v\n", err)
			return 1
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := (&agent.Daemon{SocketPath: *socket, Queue: queue, Principals: principals, SocketGID: gid}).Serve(ctx); err != nil {
			fmt.Fprintf(stderr, "agent: %v\n", err)
			return 1
		}
		return 0
	case "bridge":
		flags := flag.NewFlagSet("bridge", flag.ContinueOnError)
		flags.SetOutput(stderr)
		socket := flags.String("socket", "/run/soar/agent.sock", "agent Unix socket")
		principal := flags.String("principal", "", "fixed authorized-key principal")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return 2
		}
		if err := agent.Bridge(context.Background(), *socket, *principal, stdin, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "agent bridge: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintln(stderr, "usage: soar-agent serve|bridge")
		return 2
	}
}

func readPrincipals(path string) (map[string]agentproto.Principal, error) {
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
