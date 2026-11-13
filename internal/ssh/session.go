package ssh

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/process"
	"github.com/nchapman/soar/internal/session"
)

//go:embed session-prepare.sh
var sessionPrepareScript string

var sessionToken = regexp.MustCompile(`^[0-9a-f]{32}$`)
var sessionContainerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

type streamProcess func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error)

// StreamApp deliberately separates the artifact upload from workload stdin.
// A tar reader can read ahead; appending user input to an upload is not safe.
func StreamApp(ctx context.Context, target string, request deploy.SessionRequest, binary []byte, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return -1, err
	}
	return streamApp(ctx, target, request, binary, stdin, stdout, stderr, hex.EncodeToString(token[:]), process.Run, process.Stream)
}

func streamApp(ctx context.Context, target string, request deploy.SessionRequest, binary []byte, stdin io.Reader, stdout, stderr io.Writer, token string, run runProcess, stream streamProcess) (int, error) {
	if err := ValidateTarget(target); err != nil {
		return -1, err
	}
	if err := deploy.ValidateSessionRequest(request); err != nil {
		return -1, err
	}
	if !sessionToken.MatchString(token) {
		return -1, errors.New("invalid session token")
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > process.OutputLimit {
		return -1, errors.New("invalid or oversized session request")
	}
	payload, err := actionArchive(encoded, binary, nil, "session")
	if err != nil {
		return -1, err
	}
	// Cleanup has its own context even after Ctrl-C or a caller deadline. An
	// unreachable host is handled by inactive-stage expiry on subsequent setup.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := sessionCleanupCommand(token)
		if _, err := run(cleanupCtx, "ssh", arguments(target, command), nil); err != nil {
			fmt.Fprintln(stderr, "warning: session staging cleanup could not be confirmed; inactive staging expires on subsequent session setup or host restart")
		}
	}()
	prepareCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	script := strings.ReplaceAll(sessionPrepareScript, "__TOKEN__", token)
	command := "/bin/sh -c '" + strings.ReplaceAll(script, "'", "'\\''") + "'"
	result, runErr := run(prepareCtx, "ssh", arguments(target, command), payload)
	var prepared session.Preparation
	decodeErr := decodeJSON(result.Stdout, &prepared)
	if decodeErr == nil && prepared.Error != "" {
		return -1, fmt.Errorf("prepare session: %q", prepared.Error)
	}
	if runErr != nil {
		return -1, connectionError(target, result, runErr)
	}
	if decodeErr != nil || prepared.Version != 1 || prepared.Token != token || !validSessionTarget(request, prepared.Target) {
		return -1, errors.New("invalid session preparation response; no command attached")
	}
	cancel()
	fmt.Fprintf(stderr, "Session: app=%s service=%s container=%s candidate=%t\n", prepared.Target.App, prepared.Target.Service, prepared.Target.ContainerID, prepared.Target.Candidate)
	if !request.Stdin {
		stdin = nil
	}
	command = "/run/soar-sessions/" + token + "/executor internal-session-attach " + token
	code, err := stream(ctx, "ssh", sessionArguments(target, command, request.TTY), stdin, stdout, stderr)
	if err != nil || code == 255 {
		if request.Action == "exec" {
			return -1, errors.New("execution outcome unknown (SSH disconnected, cancelled, or exited 255); the command may still be running; do not automatically retry")
		}
		return -1, errors.New("log stream interrupted; reconnect explicitly to select the current container")
	}
	return code, nil
}

func validSessionTarget(r deploy.SessionRequest, t deploy.SessionTarget) bool {
	if t.App != r.App || t.Service == "" || (r.Service != "" && t.Service != r.Service) || t.Candidate != r.Candidate || !sessionToken.MatchString(t.HostIdentity) || !sessionToken.MatchString(t.Incarnation) || !sessionToken.MatchString(t.Operation) || !sessionContainerID.MatchString(t.ContainerID) {
		return false
	}
	if binding, ok := r.Bindings[t.Service]; ok && (binding.HostIdentity != t.HostIdentity || binding.Incarnation != t.Incarnation) {
		return false
	}
	return true
}

func sessionArguments(target, command string, tty bool) []string {
	args := arguments(target, command)
	if tty {
		args[0] = "-tt"
	}
	// Literal ~. and other OpenSSH escape sequences belong to the workload,
	// not a second hidden local command language.
	return append([]string{"-o", "EscapeChar=none"}, args...)
}

func sessionCleanupCommand(token string) string {
	// The token has already passed a fixed lowercase-hex grammar. Neither
	// application names nor command arguments ever enter shell source.
	path := "/run/soar-sessions/" + token
	return "/bin/sh -c 'if [ -e " + path + " ] || [ -L " + path + " ]; then " +
		"[ -d " + path + " ] && [ ! -L " + path + " ] && [ \"$(stat -c %u " + path + ")\" = 0 ] && [ \"$(stat -c %a " + path + ")\" = 700 ] || exit 1; " +
		path + "/executor internal-session-cleanup " + token + "; fi'"
}
