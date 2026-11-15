//go:build linux

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

type ttyWinsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func openPTY() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		master.Close()
		return nil, nil, errno
	}
	var ptyNum uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&ptyNum))); errno != 0 {
		master.Close()
		return nil, nil, errno
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptyNum), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func setWinsize(fd uintptr, size WindowSize) error {
	if size.Rows == 0 && size.Cols == 0 {
		return nil
	}
	ws := ttyWinsize{Row: size.Rows, Col: size.Cols}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); errno != 0 {
		return errno
	}
	return nil
}

func runPTY(ctx context.Context, name string, args []string, initial *WindowSize, resizes <-chan WindowSize, stdin io.Reader, stdout io.Writer) (int, error) {
	master, slave, err := openPTY()
	if err != nil {
		return -1, err
	}
	size := WindowSize{Rows: 24, Cols: 80}
	if initial != nil && (initial.Rows > 0 || initial.Cols > 0) {
		size = *initial
	}
	_ = setWinsize(master.Fd(), size)

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	// Make slave the child's controlling terminal so TIOCSWINSZ on master
	// delivers SIGWINCH to docker exec, which forwards resize to the container.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		slave.Close()
		master.Close()
		return -1, err
	}
	// Close the parent's slave descriptor immediately so master observes EIO
	// once the child exits and drains its final terminal output.
	_ = slave.Close()

	var masterMu sync.Mutex
	masterClosed := false
	closeMaster := func() {
		masterMu.Lock()
		if !masterClosed {
			masterClosed = true
			_ = master.Close()
		}
		masterMu.Unlock()
	}
	defer closeMaster()

	done := make(chan struct{})
	defer close(done)

	if resizes != nil {
		go func() {
			for {
				select {
				case <-done:
					return
				case next, ok := <-resizes:
					if !ok {
						return
					}
					masterMu.Lock()
					if !masterClosed {
						_ = setWinsize(master.Fd(), next)
					}
					masterMu.Unlock()
				}
			}
		}()
	}

	if stdin != nil {
		go func() {
			_, _ = io.Copy(master, stdin)
		}()
	}

	outDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, readErr := master.Read(buf)
			if n > 0 {
				select {
				case <-done:
					outDone <- ctx.Err()
					return
				default:
				}
				if _, writeErr := stdout.Write(buf[:n]); writeErr != nil {
					outDone <- writeErr
					return
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) || errors.Is(readErr, syscall.EIO) || errors.Is(readErr, os.ErrClosed) {
					outDone <- nil
					return
				}
				outDone <- readErr
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		closeMaster()
		select {
		case <-outDone:
		case <-time.After(250 * time.Millisecond):
		}
		return -1, ctx.Err()
	}
	outErr := <-outDone
	if outErr != nil {
		return -1, outErr
	}
	if waitErr == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) && exit.ExitCode() >= 0 {
		return exit.ExitCode(), nil
	}
	return -1, waitErr
}
