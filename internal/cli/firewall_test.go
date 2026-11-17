package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/executor"
	"github.com/nchapman/vamos/internal/firewall"
	"github.com/nchapman/vamos/internal/host"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/registry"
	"github.com/nchapman/vamos/internal/ssh"
)

func TestFirewallConfirmationHasIndependentTransportDeadline(t *testing.T) {
	var enableContext context.Context
	calls := 0
	run := func(ctx context.Context, _ string, _ executor.Request, _ []byte) (*firewall.Status, error) {
		calls++
		if calls == 1 {
			enableContext = ctx
			return &firewall.Status{Enabled: true, Pending: true, Token: "token"}, nil
		}
		if enableContext.Err() == nil {
			t.Fatal("enable transport deadline was not released before confirmation")
		}
		if ctx.Err() != nil {
			t.Fatal("confirmation inherited canceled transport context")
		}
		return &firewall.Status{Enabled: true}, nil
	}
	if _, err := enableAndConfirmFirewall(context.Background(), "host", &lifecycle.Config{}, nil, run); err != nil {
		t.Fatal(err)
	}
}

func TestFirewallChoice(t *testing.T) {
	for _, tt := range []struct {
		value, input                   string
		provided, terminal, want, fail bool
	}{
		{fail: true}, {value: "true", provided: true, want: true}, {value: "false", provided: true}, {value: "yes", provided: true, fail: true},
		{input: "yes\n", terminal: true, want: true}, {input: "no\n", terminal: true}, {input: "\n", terminal: true, fail: true},
	} {
		got, err := chooseFirewall(tt.value, tt.provided, lifecycleDeps{Input: strings.NewReader(tt.input), IsTerminal: func(io.Reader) bool { return tt.terminal }}, io.Discard)
		if (err != nil) != tt.fail || got != tt.want {
			t.Fatalf("%+v: got=%t err=%v", tt, got, err)
		}
	}
}

func TestFirewallDefaultAndFreshConfirmation(t *testing.T) {
	for _, op := range []string{"enable", "disable", "status"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", op, fail), func(t *testing.T) {
				store := registry.New(t.TempDir())
				if err := store.Put(registry.Server{Name: "home", Target: "root@home", Domain: "example.com"}); err != nil {
					t.Fatal(err)
				}
				cfg := lifecycle.Config{Name: "home", Domain: "example.com"}
				calls := 0
				deps := lifecycleDeps{
					Registry: func() (*registry.Store, error) { return store, nil },
					Status: func(context.Context, string) (*ssh.Status, error) {
						return &ssh.Status{State: &lifecycle.State{Config: cfg, Phase: "ready", ExecutorSHA: fmt.Sprintf("%x", sha256.Sum256([]byte("binary")))}}, nil
					},
					Inspect: func(context.Context, string) (*host.Report, error) {
						return &host.Report{Root: true, Supported: true, Architecture: "amd64"}, nil
					},
					Artifact: func(string, string) ([]byte, error) { return []byte("binary"), nil },
					Firewall: func(_ context.Context, target string, r executor.Request, _ []byte) (*firewall.Status, error) {
						calls++
						if target != "root@home" || r.Config == nil || *r.Config != cfg {
							t.Fatalf("request=%+v target=%s", r, target)
						}
						if op == "enable" {
							if calls == 1 {
								if r.Operation != "firewall-enable" || r.Token != "" || !r.Confirm {
									t.Fatal(r)
								}
								return &firewall.Status{Enabled: true, Pending: true, Token: "fresh-token"}, nil
							}
							if calls != 2 || r.Operation != "firewall-confirm" || r.Token != "fresh-token" || !r.Confirm {
								t.Fatal(r)
							}
						} else if r.Operation != "firewall-"+op || r.Confirm != (op != "status") {
							t.Fatal(r)
						}
						if fail {
							return nil, errors.New("connection lost")
						}
						return &firewall.Status{Enabled: op == "enable", SSHPorts: []int{22, 2222}}, nil
					},
				}
				args := []string{op}
				if op != "status" {
					args = append(args, "--yes")
				} else {
					args = append(args, "--json")
				}
				var out, diag bytes.Buffer
				code := serverFirewall(context.Background(), args, &out, &diag, deps)
				wantCalls := 1
				if op == "enable" {
					wantCalls = 2
				}
				if (code != 0) != fail || calls != wantCalls {
					t.Fatalf("code=%d calls=%d out=%s err=%s", code, calls, &out, &diag)
				}
				if fail && strings.Contains(out.String(), "enabled") {
					t.Fatal("reported success after failed confirmation")
				}
				if fail && op == "enable" && !strings.Contains(diag.String(), "rollback") {
					t.Fatal(diag.String())
				}
			})
		}
	}
}

func TestFirewallWrongFlags(t *testing.T) {
	for _, args := range [][]string{{"enable"}, {"disable"}, {"status", "--yes=false"}, {"enable", "--yes", "--json=false"}, {"status", "--token=x"}, {"confirm", "--yes"}, {"enable", "--yes", "--purge"}} {
		var out, diag bytes.Buffer
		if code := serverFirewall(context.Background(), args, &out, &diag, lifecycleDeps{}); code != 2 {
			t.Fatalf("%v code=%d", args, code)
		}
	}
	for _, op := range []string{"upgrade", "status", "teardown"} {
		args := []string{"--firewall=false", "--yes"}
		if op == "teardown" {
			args = append(args, "--purge")
		}
		args = append(args, "home")
		if code := serverLifecycle(context.Background(), op, args, io.Discard, io.Discard, lifecycleDeps{}); code != 2 {
			t.Fatalf("%s code=%d", op, code)
		}
	}
	if code := serverLifecycle(context.Background(), "add", []string{"--yes", "home", "root@home"}, io.Discard, io.Discard, lifecycleDeps{}); code != 2 {
		t.Fatalf("missing noninteractive choice code=%d", code)
	}
}

func TestServerAddFirewallChoiceAndRegistration(t *testing.T) {
	for _, choice := range []string{"true", "false"} {
		for _, confirmFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", choice, confirmFails), func(t *testing.T) {
				store := registry.New(t.TempDir())
				calls := 0
				deps := lifecycleDeps{
					Registry:  func() (*registry.Store, error) { return store, nil },
					DeployKey: func() (string, error) { return testDeployKey, nil },
					Inspect: func(context.Context, string) (*host.Report, error) {
						return &host.Report{Supported: true, Root: true, Architecture: "amd64"}, nil
					},
					Artifact: func(string, string) ([]byte, error) { return []byte("binary"), nil },
					Run: func(_ context.Context, _ string, r executor.Request, _ []byte) (*lifecycle.State, error) {
						if r.Operation != "bootstrap" {
							t.Fatal(r)
						}
						return &lifecycle.State{Phase: "ready", Config: *r.Config}, nil
					},
					Firewall: func(_ context.Context, _ string, r executor.Request, _ []byte) (*firewall.Status, error) {
						calls++
						local, err := store.Read()
						if err != nil || local.Servers["home"].Target != "root@home" {
							t.Fatal("firewall ran before registration")
						}
						if calls == 1 {
							return &firewall.Status{Enabled: true, Pending: true, Token: "token"}, nil
						}
						if r.Operation != "firewall-confirm" || r.Token != "token" {
							t.Fatal(r)
						}
						if confirmFails {
							return nil, errors.New("fresh SSH failed")
						}
						return &firewall.Status{Enabled: true}, nil
					},
				}
				var out, diag bytes.Buffer
				code := serverLifecycle(context.Background(), "add", []string{"--yes", "--firewall=" + choice, "--domain", "example.com", "home", "root@home"}, &out, &diag, deps)
				wantCalls := 0
				if choice == "true" {
					wantCalls = 2
				}
				if calls != wantCalls || (code != 0) != (choice == "true" && confirmFails) {
					t.Fatalf("code=%d calls=%d out=%s diag=%s", code, calls, &out, &diag)
				}
				if choice == "true" && confirmFails && strings.Contains(out.String(), "firewall enabled") {
					t.Fatal("false success")
				}
			})
		}
	}
}
