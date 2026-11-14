package deploy

import (
	"context"
	"errors"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

func TestBuildNeverRequestsHostNetworking(t *testing.T) {
	f, request := flowSetup(t)
	f.m.Run = func(ctx context.Context, name string, args []string, input []byte) (process.Result, error) {
		if name == "docker" && args[0] == "buildx" {
			hostNetwork := false
			for i, arg := range args {
				if arg == "--network" && i+1 < len(args) && args[i+1] == "host" {
					hostNetwork = true
				}
			}
			if hostNetwork {
				return process.Result{}, errors.New("build escaped into host networking")
			}
		}
		return f.run(ctx, name, args, input)
	}
	if _, err := f.m.Execute(context.Background(), request, []byte("context")); err != nil {
		t.Fatal(err)
	}
}

func TestContainerInventoryAcceptsStructuredPlatformMetadata(t *testing.T) {
	// Docker 29's actual `container ls --format '{{json .}}'` output includes
	// an object-valued Platform alongside the string-valued Names field.
	m := NewManager()
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{Stdout: []byte(`{"Names":"soar-hello-current","Image":"cb8598c1c7aa","Platform":{"architecture":"amd64","os":"linux"},"State":"running"}` + "\n")}, nil
	}
	found, err := m.exists(context.Background(), "container", "soar-hello-current")
	if err != nil || !found {
		t.Fatalf("found=%t error=%v", found, err)
	}
}
