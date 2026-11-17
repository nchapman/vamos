package ssh

import "strings"

// Hints are static: never interpolate SSH diagnostics as commands or silently
// relax authentication/host-key checking in response to a connection failure.
func sshFailureHint(stderr string) string {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "remote host identification has changed"):
		return "; the host key changed: verify the replacement fingerprint through a trusted channel before updating known_hosts"
	case strings.Contains(s, "host key verification failed"):
		return "; verify the host fingerprint through a trusted channel and add it to known_hosts; Vamos does not accept unknown host keys automatically"
	case strings.Contains(s, "permission denied"):
		return "; SSH authentication was rejected: check the configured User/IdentityFile and authorized key, and unlock encrypted keys in ssh-agent; password prompts are disabled"
	case strings.Contains(s, "could not resolve hostname"):
		return "; check the hostname or SSH config alias (ssh -G shows its resolved settings)"
	case strings.Contains(s, "connection refused"):
		return "; check that sshd is running and the SSH config Port is correct"
	case strings.Contains(s, "connection timed out"), strings.Contains(s, "no route to host"):
		return "; check host reachability, routing, and provider firewall/security-group rules for the SSH port"
	}
	return ""
}
