package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/nchapman/soar/internal/deploy"
)

func TestReplicaSessionFlags(t *testing.T) {
	path, _, deps := appFixture(t)
	deps.Stream = func(_ context.Context, _ string, r deploy.SessionRequest, _ []byte, _ io.Reader, out, _ io.Writer) (int, error) {
		if r.Replica != 2 {
			t.Fatalf("lost selector: %+v", r)
		}
		io.WriteString(out, "raw\x00stream")
		return 0, nil
	}
	for _, action := range []string{"exec", "console", "logs"} {
		var out, diag bytes.Buffer
		args := []string{"-f", path, "--replica", "2"}
		var code int
		if action == "logs" {
			args = append(args, "--follow")
			code = appCommand(context.Background(), action, args, &out, &diag, deps)
		} else {
			if action == "exec" {
				args = append(args, "--", "true")
			}
			code = sessionCommand(context.Background(), action, args, nil, &out, &diag, deps, func(io.Reader, io.Writer) bool { return true })
		}
		if code != 0 || out.String() != "raw\x00stream" {
			t.Fatalf("%s: code %d: %s", action, code, &diag)
		}
	}
	for _, value := range []string{"0", "-1", "9"} {
		var out bytes.Buffer
		if code := sessionCommand(context.Background(), "exec", []string{"--replica", value, "--", "true"}, nil, &out, &out, appDeps{}, terminalStreams); code != 2 {
			t.Fatalf("accepted selector %s", value)
		}
	}
}

func TestReplicaStatusPresentation(t *testing.T) {
	for _, width := range []int{48, 80, 120} {
		out := newHelpAppsTerminal(width, true)
		s := presentationStatusFixture()
		ss := s.Services["web"]
		ss.Container, ss.Candidate = "", ""
		ss.DesiredInstances, ss.CurrentInstances, ss.RunningInstances, ss.AvailableInstances = 3, 2, 1, 1
		ss.Running, ss.Available, ss.Unavailable = false, false, true
		ss.Instances = []deploy.InstanceStatus{{Ordinal: 1, Container: "current-one", Port: "31001", Running: true, Available: true}, {Ordinal: 2, Container: "current-two", Port: "31002"}}
		ss.CandidateInstances = []deploy.InstanceStatus{{Ordinal: 1, Container: "candidate-one", Port: "31003", Running: true}, {Ordinal: 2, Container: "candidate-two", Port: "31004"}, {Ordinal: 3, Container: "candidate-three", Port: "31005"}}
		s.Services["web"] = ss
		r := presentationReportFixture()
		r.service = "web"
		text := ansi.Strip(out.p.statusReport(s, r))
		for _, want := range []string{"Desired 3 / Current 2 / Running 1 / Available 1", "Replica 1", "Replica 2", "Candidate replica 3", "current-one", "current-two", "candidate-three", "31001", "31005", "--candidate --replica 1"} {
			if !strings.Contains(text, want) {
				t.Errorf("width %d missing %q: %s", width, want, text)
			}
		}
		if strings.Contains(text, "database-retired") || strings.Contains(text, "healthy") {
			t.Fatal(text)
		}
	}
}

func TestReplicaSingletonPlainCompatibility(t *testing.T) {
	path, _, deps := appFixture(t)
	s := presentationStatusFixture()
	delete(s.Services, "database")
	deps.Run = func(context.Context, string, deploy.Request, []byte, []byte) (deploy.Response, error) {
		return deploy.Response{Version: 1, Status: s}, nil
	}
	var before, after, diag bytes.Buffer
	if code := appCommand(context.Background(), "status", []string{"-f", path}, &before, &diag, deps); code != 0 {
		t.Fatal(code, &diag)
	}
	ss := s.Services["web"]
	ss.DesiredInstances, ss.CurrentInstances, ss.RunningInstances, ss.AvailableInstances = 1, 1, 1, 1
	ss.Instances = []deploy.InstanceStatus{{Ordinal: 1, Container: ss.Container, Running: true, Available: true}}
	s.Services["web"] = ss
	if code := appCommand(context.Background(), "status", []string{"-f", path}, &after, &diag, deps); code != 0 {
		t.Fatal(code, &diag)
	}
	if before.String() != after.String() {
		t.Fatalf("singleton plain output changed:\n%s", &after)
	}
}

func TestReplicaLogsHelp(t *testing.T) {
	var out, diag bytes.Buffer
	if code := appCommand(context.Background(), "logs", []string{"--help"}, &out, &diag, appDeps{}); code != 0 {
		t.Fatal(code, &diag)
	}
	for _, want := range []string{"--replica", "multi-instance"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q: %s", want, &out)
		}
	}
}
