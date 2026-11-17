// Exercises the uploaded administrative transport, independently of the default
// restricted coordinator transport exercised by the CLI integration scenario.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/ssh"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if runtime.GOOS != "linux" || cwd != "/opt/vamos-test" || len(os.Args) != 2 {
		return fmt.Errorf("requires disposable integration guest and app argument")
	}
	if _, err := os.Stat("/var/lib/vamos-integration-owner"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	binary, err := os.ReadFile(filepath.Join(cwd, "bin", "vamos-linux-"+runtime.GOARCH))
	if err != nil {
		return err
	}
	target := "vamosadmin@127.0.0.1"
	report, err := ssh.InspectAdmin(ctx, target)
	if err != nil {
		return err
	}
	if !report.Root || !report.Supported {
		return fmt.Errorf("sudo administrative inspection failed: %+v", report)
	}
	host := lifecycle.Config{Name: "local", Domain: "vamos.localhost"}
	response, err := ssh.RunApp(ctx, target, deploy.Request{Version: 1, Action: "status", Host: host, App: os.Args[1]}, binary, nil)
	if err != nil {
		return err
	}
	if response.Status == nil {
		return fmt.Errorf("missing uploaded status response")
	}
	if err := runtimeFailures(ctx, target, response.Status.Services["web"].Image, "vamos-"+os.Args[1]); err != nil {
		return err
	}
	input := []byte("sudo session stdin\x00with binary data\n")
	var out, stderr bytes.Buffer
	code, err := ssh.StreamApp(ctx, target, deploy.SessionRequest{Version: 1, Action: "exec", Host: host, App: os.Args[1], Service: "web", Stdin: true, Command: []string{"python", "-c", "import sys; sys.stdout.buffer.write(sys.stdin.buffer.read())"}}, binary, bytes.NewReader(input), &out, &stderr)
	if err != nil {
		return err
	}
	if code != 0 || !bytes.Equal(input, out.Bytes()) {
		return fmt.Errorf("uploaded sudo session failed: code=%d output=%q stderr=%q", code, out.Bytes(), stderr.String())
	}
	fmt.Println("Uploaded sudo status/session/stdin/cleanup and runtime error classification passed")
	return nil
}
