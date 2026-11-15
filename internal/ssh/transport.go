package ssh

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/transport"
)

// Transport implements transport.Client using per-command SSH executor staging.
type Transport struct {
	ExecutorOverride string
	InspectFn        func(context.Context, string) (*host.Report, error)
	ArtifactFn       func(string, string) ([]byte, error)
	RunAppFn         func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error)
	StreamAppFn      func(context.Context, string, deploy.SessionRequest, []byte, io.Reader, io.Writer, io.Writer) (int, error)
}

var _ transport.Client = (*Transport)(nil)

func NewTransport(executorOverride string) *Transport {
	return &Transport{
		ExecutorOverride: executorOverride,
		InspectFn:        Inspect,
		ArtifactFn:       Artifact,
		RunAppFn:         RunApp,
		StreamAppFn:      StreamApp,
	}
}

func (t *Transport) Inspect(ctx context.Context, target string) (*host.Report, error) {
	fn := t.InspectFn
	if fn == nil {
		fn = Inspect
	}
	return fn(ctx, target)
}

func (t *Transport) prepareArtifact(ctx context.Context, target string) ([]byte, error) {
	inspectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	report, err := t.Inspect(inspectCtx, target)
	cancel()
	if err != nil {
		return nil, err
	}
	if !report.Supported || !report.Root {
		return nil, errors.New("deployment requires root on a supported Ubuntu host")
	}
	artifactFn := t.ArtifactFn
	if artifactFn == nil {
		artifactFn = Artifact
	}
	return artifactFn(t.ExecutorOverride, report.Architecture)
}

func (t *Transport) RunApp(ctx context.Context, target string, request deploy.Request, payload []byte) (deploy.Response, error) {
	binary, err := t.prepareArtifact(ctx, target)
	if err != nil {
		return deploy.Response{}, err
	}
	fn := t.RunAppFn
	if fn == nil {
		fn = RunApp
	}
	return fn(ctx, target, request, binary, payload)
}

func (t *Transport) StreamApp(ctx context.Context, target string, request deploy.SessionRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	binary, err := t.prepareArtifact(ctx, target)
	if err != nil {
		return -1, err
	}
	fn := t.StreamAppFn
	if fn == nil {
		fn = StreamApp
	}
	return fn(ctx, target, request, binary, stdin, stdout, stderr)
}
