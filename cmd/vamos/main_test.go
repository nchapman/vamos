package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestShutdownSignalHelper(t *testing.T) {
	if os.Getenv("VAMOS_TEST_SIGNAL_HELPER") != "1" {
		return
	}
	ctx, stop := shutdownContext()
	fmt.Println("ready")
	<-ctx.Done()
	fmt.Println(ctx.Err())
	stop()
	os.Exit(0)
}

func TestTerminationSignalsCancelContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support sending Unix process signals")
	}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShutdownSignalHelper$")
			cmd.Env = append(os.Environ(), "VAMOS_TEST_SIGNAL_HELPER=1")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() || scanner.Text() != "ready" {
				cancel()
				cmd.Wait()
				t.Fatalf("signal helper did not become ready: %s", &stderr)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				cancel()
				cmd.Wait()
				t.Fatal(err)
			}
			cancelled := scanner.Scan() && scanner.Text() == "context canceled"
			if err := cmd.Wait(); err != nil || !cancelled {
				t.Fatalf("signal %s failed to cancel cleanly: cancelled=%t err=%v stderr=%s", sig, cancelled, err, &stderr)
			}
		})
	}
}
