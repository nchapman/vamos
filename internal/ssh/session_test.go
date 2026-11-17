package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/process"
	"github.com/nchapman/vamos/internal/session"
)

func sessionFixture() (deploy.SessionRequest, session.Preparation) {
	r := deploy.SessionRequest{Version: 1, Action: "exec", Host: lifecycle.Config{Name: "home", Domain: "example.com"}, App: "hello", Service: "web", Command: []string{"printf", "", "$(touch /tmp/not-run)", "two words", "\"; café"}}
	p := session.Preparation{Version: 1, Token: strings.Repeat("a", 32), Target: deploy.SessionTarget{HostIdentity: strings.Repeat("b", 32), App: "hello", Service: "web", Incarnation: strings.Repeat("c", 32), Operation: strings.Repeat("d", 32), ContainerID: strings.Repeat("e", 64), Container: "vamos-hello-web"}}
	return r, p
}

func TestSessionTransportSeparatesSetupAndWorkloadStreams(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(map[bool]string{false: "pipe", true: "terminal"}[tty], func(t *testing.T) {
			r, p := sessionFixture()
			r.Stdin, r.TTY = true, tty
			input := strings.NewReader("binary\x00input")
			var stdout, stderr bytes.Buffer
			calls := 0
			run := func(ctx context.Context, name string, args []string, payload []byte) (process.Result, error) {
				calls++
				if ctx.Err() != nil || name != "ssh" || args[0] != "-T" {
					t.Fatalf("invalid control call %v", args)
				}
				if calls == 2 {
					if args[len(args)-1] != adminCommand(sessionCleanupCommand(p.Token)) {
						t.Fatal("cleanup not elevated")
					}
					if payload != nil || !strings.Contains(args[len(args)-1], "internal-session-cleanup") {
						t.Fatal("missing cleanup")
					}
					return process.Result{}, nil
				}
				if args[len(args)-1] != adminCommand("/bin/sh -c "+shellQuote(strings.ReplaceAll(sessionPrepareScript, "__TOKEN__", p.Token))) {
					t.Fatal("preparation not elevated")
				}
				var uploaded deploy.SessionRequest
				tr := tar.NewReader(bytes.NewReader(payload))
				for {
					h, err := tr.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if h.Name == "request.json" {
						if err := json.NewDecoder(tr).Decode(&uploaded); err != nil {
							t.Fatal(err)
						}
					}
				}
				if !reflect.DeepEqual(uploaded, r) {
					t.Fatalf("uploaded=%+v", uploaded)
				}
				if strings.Contains(args[len(args)-1], "touch /tmp") || strings.Contains(args[len(args)-1], "two words") {
					t.Fatal("user argv entered shell source")
				}
				data, _ := json.Marshal(p)
				return process.Result{Stdout: data}, nil
			}
			stream := func(_ context.Context, name string, args []string, stdin io.Reader, out, diag io.Writer) (int, error) {
				wantTTY := "-T"

				joined := strings.Join(args, " ")
				if name != "ssh" || args[2] != wantTTY || stdin != input || out != &stdout || diag != &stderr {
					t.Fatalf("bad session streams/args %v", args)
				}
				for _, option := range []string{"EscapeChar=none", "StrictHostKeyChecking=yes", "ForwardAgent=no", "ClearAllForwardings=yes", "PermitLocalCommand=no", "ControlPath=none"} {
					if !strings.Contains(joined, option) {
						t.Fatalf("missing %s", option)
					}
				}
				if strings.Contains(joined, "touch /tmp") || args[len(args)-1] != adminCommand("/var/lib/.vamos-session-exec."+p.Token+" internal-session-attach "+p.Token) {
					t.Fatal("unsafe attach command")
				}
				io.WriteString(out, "workload output")
				return 42, nil
			}
			code, err := streamApp(context.Background(), "root@home", r, []byte("executor"), input, &stdout, &stderr, p.Token, run, stream)
			if code != 42 || err != nil || calls != 2 || stdout.String() != "workload output" {
				t.Fatalf("code=%d err=%v calls=%d out=%q", code, err, calls, &stdout)
			}
		})
	}
}

func TestSessionDisconnectUsesFreshCleanupAndNeverRetries(t *testing.T) {
	for _, exit := range []int{-1, 255} {
		r, p := sessionFixture()
		ctx, cancel := context.WithCancel(context.Background())
		calls, streams := 0, 0
		run := func(ctx context.Context, _ string, _ []string, _ []byte) (process.Result, error) {
			calls++
			if ctx.Err() != nil {
				t.Fatal("cleanup inherited cancellation")
			}
			data, _ := json.Marshal(p)
			return process.Result{Stdout: data}, nil
		}
		stream := func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
			streams++
			cancel()
			if exit == -1 {
				return -1, errors.New("disconnected")
			}
			return 255, nil
		}
		code, err := streamApp(ctx, "root@home", r, []byte("executor"), nil, io.Discard, io.Discard, p.Token, run, stream)
		if code != -1 || err == nil || !strings.Contains(err.Error(), "outcome unknown") || calls != 2 || streams != 1 {
			t.Fatalf("code=%d err=%v calls=%d streams=%d", code, err, calls, streams)
		}
	}
}

func TestSessionBadPreparationNeverAttaches(t *testing.T) {
	for _, mode := range []string{"token", "container", "host binding", "malformed", "remote error"} {
		t.Run(mode, func(t *testing.T) {
			r, p := sessionFixture()
			token := p.Token
			switch mode {
			case "token":
				p.Token = "../escape"
			case "container":
				p.Target.ContainerID = "container-name"
			case "host binding":
				r.Bindings = map[string]deploy.SessionBinding{"web": {HostIdentity: strings.Repeat("f", 32), Incarnation: p.Target.Incarnation}}
			case "remote error":
				p.Error = "target unavailable"
			}
			data, _ := json.Marshal(p)
			if mode == "malformed" {
				data = []byte("garbage")
			}
			calls := 0
			run := func(context.Context, string, []string, []byte) (process.Result, error) {
				calls++
				return process.Result{Stdout: data}, nil
			}
			stream := func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
				t.Fatal("attached invalid preparation")
				return 0, nil
			}
			if _, err := streamApp(context.Background(), "root@home", r, []byte("executor"), nil, io.Discard, io.Discard, token, run, stream); err == nil || calls != 2 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestSessionDoesNotForwardInputWithoutOptIn(t *testing.T) {
	r, p := sessionFixture()
	data, _ := json.Marshal(p)
	run := func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{Stdout: data}, nil
	}
	stream := func(_ context.Context, _ string, _ []string, stdin io.Reader, _, _ io.Writer) (int, error) {
		if stdin != nil {
			t.Fatal("unexpected stdin forwarding")
		}
		return 0, nil
	}
	if code, err := streamApp(context.Background(), "root@home", r, []byte("executor"), strings.NewReader("private input"), io.Discard, io.Discard, p.Token, run, stream); code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
}
