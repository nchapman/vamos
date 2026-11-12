package host

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func frame(kernel, arch, uid, init, manager, status, metadata string) []byte {
	return []byte(strings.Join([]string{"soar-inspect-v1", kernel, arch, uid, init, manager, status, metadata}, "\n"))
}

func ubuntu(version string) []byte {
	return frame("Linux", "x86_64", "1000", "systemd", "yes", "present", "ID=ubuntu\nVERSION_ID=\""+version+"\"\n")
}

func TestSupportedHosts(t *testing.T) {
	for _, version := range []string{"24.04", "24.10", "26.04", "30.10"} {
		t.Run(version, func(t *testing.T) {
			report, err := Parse(ubuntu(version))
			if err != nil || !report.Supported || report.Root || report.UID != 1000 || report.Architecture != "amd64" {
				t.Fatalf("report=%+v error=%v", report, err)
			}
		})
	}
	t.Run("arm64 root with single quotes", func(t *testing.T) {
		r, err := Parse(frame("Linux", "aarch64", "0", "systemd", "yes", "present", "# comment\nID='ubuntu'\nVERSION_ID='24.04'\nPRETTY_NAME=\"Ubuntu 24.04 LTS\"\n"))
		if err != nil || !r.Supported || !r.Root || r.Architecture != "arm64" {
			t.Fatalf("report=%+v error=%v", r, err)
		}
	})
}

func TestUnsupportedHostsRetainFacts(t *testing.T) {
	tests := []struct{ name, old, replacement, want string }{
		{"older Ubuntu", "24.04", "22.04", "24.04 or newer"},
		{"invalid month", "24.04", "26.99", "24.04 or newer"},
		{"unknown version", "24.04", "rolling", "unrecognized VERSION_ID"},
		{"derivative", "ID=ubuntu", "ID=linuxmint\nID_LIKE=ubuntu", "ID=ubuntu"},
		{"non-Linux", "Linux\n", "Darwin\n", "must run Linux"},
		{"architecture", "x86_64", "riscv64", "amd64 or arm64"},
		{"non-systemd PID1", "systemd\nyes", "bash\nyes", "systemd must be PID 1"},
		{"no manager", "systemd\nyes", "systemd\nno", "systemd must be PID 1"},
		{"duplicate ID", "ID=ubuntu", "ID=debian\nID=ubuntu", "duplicate ID"},
		{"duplicate version", "VERSION_ID=\"24.04\"", "VERSION_ID=\"24.04\"\nVERSION_ID=\"26.04\"", "duplicate VERSION_ID"},
		{"missing ID", "ID=ubuntu", "ID_LIKE=ubuntu", "ID and VERSION_ID"},
		{"shell expression", "ID=ubuntu", "ID=\"$(echo ubuntu)\"", "unrecognized ID"},
		{"malformed metadata", "ID=ubuntu", "not an assignment", "assignment lines"},
		{"NUL metadata", "ID=ubuntu", "ID=ubuntu\x00", "NUL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Replace(string(ubuntu("24.04")), tt.old, tt.replacement, 1)
			r, err := Parse([]byte(input))
			if err != nil || r.Supported || !strings.Contains(strings.Join(r.Problems, ";"), tt.want) || r.UID != 1000 {
				t.Fatalf("report=%+v err=%v; want unsupported reason %q", r, err, tt.want)
			}
		})
	}
	t.Run("missing metadata", func(t *testing.T) {
		r, err := Parse(frame("Linux", "x86_64", "0", "systemd", "yes", "missing", ""))
		if err != nil || r.Supported || !strings.Contains(strings.Join(r.Problems, ";"), "missing or unreadable") {
			t.Fatalf("report=%+v err=%v", r, err)
		}
	})
}

func TestMalformedProbeOutput(t *testing.T) {
	good := string(ubuntu("24.04"))
	for name, input := range map[string]string{
		"empty":              "",
		"banner":             "Welcome\n" + good,
		"truncated":          "soar-inspect-v1\nLinux\n",
		"wrong version":      strings.Replace(good, "v1", "v2", 1),
		"bad scalar":         strings.Replace(good, "x86_64", "x86_64\x1b[31m", 1),
		"bad UID":            strings.Replace(good, "1000", "user", 1),
		"overflow UID":       strings.Replace(good, "1000", "4294967296", 1),
		"bad evidence":       strings.Replace(good, "yes", "perhaps", 1),
		"bad status":         strings.Replace(good, "present", "absent", 1),
		"missing with data":  strings.Replace(good, "present", "missing", 1),
		"oversized metadata": good + strings.Repeat("x", maxOSRelease),
	} {
		t.Run(name, func(t *testing.T) {
			if r, err := Parse([]byte(input)); err == nil || r != nil {
				t.Fatalf("accepted malformed probe: report=%+v err=%v", r, err)
			}
		})
	}
}

func TestInspectDoesNotParseFailedCollection(t *testing.T) {
	failure := errors.New("connection failed")
	r, err := Inspect(context.Background(), func(context.Context) ([]byte, error) { return ubuntu("24.04"), failure })
	if r != nil || !errors.Is(err, failure) {
		t.Fatalf("report=%+v err=%v", r, err)
	}
}

func TestCollectorOnNativeHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("collector runs on remote Linux hosts, not Windows clients")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-s")
	cmd.Stdin = strings.NewReader(Collector())
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(output)
	if err != nil {
		t.Fatalf("parse native collector: %v", err)
	}
	if r.Kernel == "" || r.Architecture == "" {
		t.Fatalf("missing facts: %+v", r)
	}
	if runtime.GOOS == "darwin" && r.Supported {
		t.Fatal("macOS is a supported client, not a supported deployment host")
	}
}
