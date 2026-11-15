//go:build !windows

package ssh

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/nchapman/vamos/internal/agentproto"
	"golang.org/x/term"
)

func terminalFD(stdin io.Reader, stdout io.Writer) (int, bool) {
	if out, ok := stdout.(*os.File); ok && term.IsTerminal(int(out.Fd())) {
		return int(out.Fd()), true
	}
	if in, ok := stdin.(*os.File); ok && term.IsTerminal(int(in.Fd())) {
		return int(in.Fd()), true
	}
	return -1, false
}

func readWindowSize(fd int) (agentproto.WindowSize, bool) {
	cols, rows, err := term.GetSize(fd)
	if err != nil || cols <= 0 || rows <= 0 {
		return agentproto.WindowSize{}, false
	}
	return agentproto.WindowSize{
		Rows: uint16(min(rows, 65535)),
		Cols: uint16(min(cols, 65535)),
	}, true
}

func initialWindowSize(stdin io.Reader, stdout io.Writer) *agentproto.WindowSize {
	fd, ok := terminalFD(stdin, stdout)
	if !ok {
		return nil
	}
	size, ok := readWindowSize(fd)
	if !ok {
		return nil
	}
	return &size
}

func watchWindowSize(ctx context.Context, stdin io.Reader, stdout io.Writer, initial *agentproto.WindowSize) (<-chan agentproto.WindowSize, func()) {
	fd, ok := terminalFD(stdin, stdout)
	if !ok {
		return nil, func() {}
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	out := make(chan agentproto.WindowSize, 4)
	done := make(chan struct{})
	// Re-sample after registering SIGWINCH so a resize during SSH handshake is not missed.
	if current, ok := readWindowSize(fd); ok && (initial == nil || current != *initial) {
		out <- current
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-sigCh:
				if next, ok := readWindowSize(fd); ok {
					select {
					case out <- next:
					default:
						// Coalesce bursty resize signals to the newest dimensions.
						select {
						case <-out:
						default:
						}
						select {
						case out <- next:
						default:
						}
					}
				}
			}
		}
	}()
	return out, func() {
		signal.Stop(sigCh)
		close(done)
	}
}
