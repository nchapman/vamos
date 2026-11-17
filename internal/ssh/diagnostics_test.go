package ssh

import (
	"strings"
	"testing"
)

func TestSSHFailureHintsDoNotWeakenSecurity(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"REMOTE HOST IDENTIFICATION HAS CHANGED!", "trusted channel"},
		{"Host key verification failed.", "does not accept unknown host keys"},
		{"Permission denied (publickey).", "password prompts are disabled"},
		{"Could not resolve hostname test", "SSH config alias"},
		{"Connection refused", "sshd"},
		{"Connection timed out", "security-group"},
	} {
		if got := sshFailureHint(tc.input); !strings.Contains(got, tc.want) {
			t.Fatalf("%q: %q", tc.input, got)
		}
	}
	if got := sshFailureHint("arbitrary output; run evil-command"); got != "" {
		t.Fatal(got)
	}
}

func TestTargetPortShowsSSHConfigRemedy(t *testing.T) {
	err := ValidateTarget("ubuntu@example.com:2222")
	if err == nil || !strings.Contains(err.Error(), "HostName and Port") {
		t.Fatal(err)
	}
	if err := ValidateTarget("ubuntu@2001:db8::1"); err != nil {
		t.Fatal(err)
	}
}
