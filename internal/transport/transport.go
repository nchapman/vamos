// Package transport defines the client-to-host RPC seam separating CLI
// workflows from SSH executor uploads and future soar-agent socket clients.
package transport

import (
	"context"
	"io"

	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/host"
)

type Client interface {
	Inspect(ctx context.Context, target string) (*host.Report, error)
	RunApp(ctx context.Context, target string, request deploy.Request, payload []byte) (deploy.Response, error)
	StreamApp(ctx context.Context, target string, request deploy.SessionRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}
