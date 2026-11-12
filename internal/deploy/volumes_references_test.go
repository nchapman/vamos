package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

func TestVolumeForeignStoppedReferenceBlocksCreation(t *testing.T) {
	m, j, rec := volumeBoundaryFixture(t)
	m.Run = func(_ context.Context, _ string, args []string, _ []byte) (process.Result, error) {
		cmd := strings.Join(args, " ")
		switch cmd {
		case "volume ls --format {{json .}}":
			return process.Result{}, nil
		case "container ls --all --filter volume=" + rec.DockerName() + " --format {{json .}}":
			return process.Result{Stdout: []byte(`{"Names":"foreign"}`)}, nil
		case "container inspect foreign":
			return process.Result{Stdout: []byte(`[{"Name":"/foreign","Config":{"Labels":{}},"State":{"Running":false},"HostConfig":{"RestartPolicy":{"Name":"no"}}}]`)}, nil
		default:
			t.Fatalf("must not mutate Docker: %s", cmd)
			return process.Result{}, nil
		}
	}
	if err := m.ensureVolumes(context.Background(), j); err == nil {
		t.Fatal("stopped foreign reference accepted")
	}
}
