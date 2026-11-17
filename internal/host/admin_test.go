package host

import (
	"strings"
	"testing"
)

func TestRequireAdminExplainsSpecificPrerequisite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report *Report
		want   string
	}{
		{"missing", nil, "no report"},
		{"unsupported", &Report{Problems: []string{"Ubuntu VERSION_ID must be 24.04 or newer", "systemd must be PID 1"}}, "24.04 or newer; systemd"},
		{"nonroot", &Report{Supported: true, UID: 1000}, "passwordless sudo"},
		{"ready", &Report{Supported: true, Root: true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RequireAdmin(tc.report)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
