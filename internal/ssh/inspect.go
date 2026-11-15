// Package ssh implements fresh-host inspection through the client's OpenSSH.
package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/process"
)

var (
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	userPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
)

// ValidateTarget accepts SSH aliases and [user@]host, including unbracketed IPv6.
// Ports, keys, and jump hosts belong in the user's trusted SSH configuration.
func ValidateTarget(target string) error {
	if len(target) == 0 || len(target) > 512 || strings.Count(target, "@") > 1 {
		return errors.New("target must be an SSH alias or [user@]host; configure ports and keys in SSH config")
	}
	hostname := target
	if user, rest, found := strings.Cut(target, "@"); found {
		if !userPattern.MatchString(user) {
			return errors.New("invalid SSH username; use a simple username or an SSH alias")
		}
		hostname = rest
	}
	if !hostPattern.MatchString(hostname) && net.ParseIP(hostname) == nil {
		return errors.New("invalid SSH host; use an alias, hostname, IPv4, or unbracketed IPv6 address")
	}
	return nil
}

func Inspect(ctx context.Context, target string) (*host.Report, error) {
	return inspect(ctx, target, process.Run)
}

type runProcess func(context.Context, string, []string, []byte) (process.Result, error)

func inspect(ctx context.Context, target string, run runProcess) (*host.Report, error) {
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}
	args := arguments(target, "/bin/sh -s")
	return host.Inspect(ctx, func(ctx context.Context) ([]byte, error) {
		result, err := run(ctx, "ssh", args, []byte(host.Collector()))
		if err != nil {
			return nil, connectionError(target, result, err)
		}
		return result.Stdout, nil
	})
}

func arguments(target, command string) []string {
	return []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UpdateHostKeys=no",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "PermitLocalCommand=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "RemoteCommand=none",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=1",
		"--", target, command,
	}
}

func connectionError(target string, result process.Result, err error) error {
	detail := ""
	if len(result.Stderr) > 0 {
		stderr := result.Stderr
		if len(stderr) > 4096 {
			stderr = append(append([]byte{}, stderr[:4096]...), []byte(" [truncated]")...)
		}
		detail = fmt.Sprintf("; SSH stderr: %q", stderr)
	}
	return fmt.Errorf("connect to %q over SSH: %w%s; verify OpenSSH, connectivity, known_hosts, and noninteractive key/agent authentication", target, err, detail)
}
