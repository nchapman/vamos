//go:build windows

package ssh

import (
	"context"
	"io"
	"os"

	"github.com/nchapman/vamos/internal/agentproto"
	"golang.org/x/term"
)

func initialWindowSize(stdin io.Reader, stdout io.Writer) *agentproto.WindowSize {
	for _, fdFn := range []func() (int, bool){
		func() (int, bool) {
			f, ok := stdout.(*os.File)
			if !ok {
				return -1, false
			}
			return int(f.Fd()), term.IsTerminal(int(f.Fd()))
		},
		func() (int, bool) {
			f, ok := stdin.(*os.File)
			if !ok {
				return -1, false
			}
			return int(f.Fd()), term.IsTerminal(int(f.Fd()))
		},
	} {
		if fd, ok := fdFn(); ok {
			if cols, rows, err := term.GetSize(fd); err == nil && cols > 0 && rows > 0 {
				size := agentproto.WindowSize{Rows: uint16(min(rows, 65535)), Cols: uint16(min(cols, 65535))}
				return &size
			}
		}
	}
	return nil
}

func watchWindowSize(context.Context, io.Reader, io.Writer, *agentproto.WindowSize) (<-chan agentproto.WindowSize, func()) {
	return nil, func() {}
}
