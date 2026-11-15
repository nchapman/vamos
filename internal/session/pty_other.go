//go:build !linux

package session

import (
	"context"
	"errors"
	"io"
)

func runPTY(context.Context, string, []string, *WindowSize, <-chan WindowSize, io.Reader, io.Writer) (int, error) {
	return -1, errors.New("PTY allocation requires a Linux host")
}
