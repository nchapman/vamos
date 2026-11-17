package engine

import (
	"errors"
	"regexp"
	"strings"
)

// Runtime failures contain only local text: never attach the process error,
// arguments, or output. Diagnostics are untrusted and are not recovery authority.
var (
	ErrPortBind          = errors.New("Docker could not bind a host port. Inspect host port usage and the configured published ports before retrying")
	ErrDaemonUnavailable = errors.New("Docker daemon is unreachable. Check that Docker is running and that the configured Docker endpoint is accessible")
	ErrImageAccess       = errors.New("Docker could not access the image. Verify the image reference, registry availability, and registry credentials")
	ErrDiskFull          = errors.New("Docker storage is full. Inspect host disk space and inode usage before retrying; no data was automatically removed")
	ErrRuntime           = errors.New("Docker command failed; runtime diagnostic output withheld")
)

var runtimeClasses = []struct {
	err     error
	code    string
	pattern *regexp.Regexp
}{
	{ErrPortBind, "docker_port_bind", regexp.MustCompile(`^Error response from daemon: ((failed to set up container networking: )?driver failed programming external connectivity on endpoint [^\r\n]+: (Bind for [^\r\n]+ failed: port is already allocated|failed to bind (host )?port [^\r\n]+: address already in use)|ports are not available: exposing port [^\r\n]+: bind: address already in use)\.?$`)},
	{ErrDaemonUnavailable, "docker_daemon_unavailable", regexp.MustCompile(`^Cannot connect to the Docker daemon at [^\r\n]+\. Is the docker daemon running\?$`)},
	{ErrImageAccess, "docker_image_access", regexp.MustCompile(`^Error response from daemon: (No such image: [^\r\n]+|pull access denied for [^\r\n]+, repository does not exist or may require 'docker login'(: denied: requested access to the resource is denied)?|manifest for [^\r\n]+ not found: manifest unknown: manifest unknown|unauthorized: authentication required|Get "[^\r\n]+": (unauthorized: authentication required|denied: requested access to the resource is denied))$`)},
	{ErrDiskFull, "docker_disk_full", regexp.MustCompile(`^Error response from daemon: (write|mkdir|open) [^\r\n]+: no space left on device$`)},
}

// ClassifyRuntimeFailure examines only a bounded first diagnostic line, not
// stdout (which may be application output), embedded lines, or arbitrary error
// values. Matching values are discarded, never interpolated into a hint.
func ClassifyRuntimeFailure(stderr []byte) error {
	if len(stderr) > 16384 {
		return ErrRuntime
	}
	line, _, _ := strings.Cut(string(stderr), "\n")
	line = strings.TrimSuffix(line, "\r")
	// docker run commonly adds this CLI prefix to daemon errors.
	line = strings.TrimPrefix(line, "docker: ")
	line = strings.TrimSuffix(line, ".")
	// Preserve the punctuation required by the daemon-unreachable signature.
	for _, c := range runtimeClasses {
		if c.pattern.MatchString(line) {
			return c.err
		}
	}
	return ErrRuntime
}

// RuntimeErrorCode classifies identities, not error strings.
func RuntimeErrorCode(err error) string {
	for _, c := range runtimeClasses {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return ""
}
