// Package transport defines the client-to-host RPC seam separating CLI
// workflows from SSH executor uploads and future vamos-agent socket clients.
package transport

import (
	"context"
	"io"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/host"
)

type Client interface {
	Inspect(ctx context.Context, target string) (*host.Report, error)
	RunApp(ctx context.Context, target string, request deploy.Request, payload []byte) (deploy.Response, error)
	StreamApp(ctx context.Context, target string, request deploy.SessionRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}
