package firewall

import (
	"context"
	"errors"
	"fmt"
	"github.com/nchapman/vamos/internal/process"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestNFTCompatibilityInventory(t *testing.T) {
	saves := map[string]string{"ip": "*filter\n:INPUT ACCEPT [0:0]\nCOMMIT\n"}
	good := `{"nftables":[{"table":{"family":"ip","name":"filter"}},{"chain":{"family":"ip","table":"filter","name":"INPUT","type":"filter","hook":"input","prio":0,"policy":"accept"}}]}`
	if err := validateNFT(good, saves); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(good, `"prio":0`, `"prio":-1`, 1),
		strings.Replace(good, `"family":"ip"`, `"family":"inet"`, 1),
		strings.Replace(good, `"INPUT"`, `"FOREIGN"`, 1),
		`{"nftables":[{"set":{"family":"ip","table":"filter","name":"foreign"}}]}`,
		strings.TrimSuffix(good, "]}") + `,{"rule":{"family":"ip","table":"filter","chain":"INPUT","expr":[]}}]}`,
	} {
		if err := validateNFT(bad, saves); err == nil {
			t.Fatalf("accepted native/hidden nft policy: %s", bad)
		}
	}
}
func TestNFTAllowsSyntheticEmptyAcceptBuiltins(t *testing.T) {
	// iptables-nft-save includes empty ACCEPT builtins even before those
	// base chains have been materialized in the native nftables ruleset.
	saves := map[string]string{"ip": "*filter\n:INPUT ACCEPT [0:0]\n:OUTPUT ACCEPT [0:0]\nCOMMIT\n"}
	if err := validateNFT(`{"nftables":[]}`, saves); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"*filter\n:INPUT DROP [0:0]\nCOMMIT\n", "*filter\n:DOCKER - [0:0]\nCOMMIT\n", "*filter\n:INPUT ACCEPT [0:0]\n-A INPUT -j DROP\nCOMMIT\n"} {
		if err := validateNFT(`{"nftables":[]}`, map[string]string{"ip": text}); err == nil {
			t.Fatal("accepted missing effective policy", text)
		}
	}
}

func TestDockerRawEndpointOwnership(t *testing.T) {
	endpoints := map[string]string{"172.17.0.2/32": "docker0"}
	for _, line := range []string{"-A PREROUTING -d 172.17.0.2/32 ! -i docker0 -j DROP", "-A PREROUTING -d 172.17.0.2/32 ! -i docker0 -p tcp -m tcp --dport 8080 -j DROP"} {
		if !validDockerRaw(line, endpoints) {
			t.Fatal(line)
		}
	}
	for _, line := range []string{"-A PREROUTING -d 172.17.0.3/32 ! -i docker0 -j DROP", "-A PREROUTING -d 172.17.0.2/32 ! -i eth0 -j DROP", "-A PREROUTING -d 172.17.0.2/32 ! -i docker0 -p tcp -m tcp --dport 0 -j DROP"} {
		if validDockerRaw(line, endpoints) {
			t.Fatal(line)
		}
	}
}
func TestRoutingPolicyRejected(t *testing.T) {
	good := `[{"priority":0,"src":"all","table":"local"},{"priority":32766,"src":"all","table":"main"}]`
	if err := validateRoutingRules(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`[]`, strings.Replace(good, `"all"`, `"10.0.0.0/8"`, 1), strings.Replace(good, `"src":"all"`, `"fwmark":"0x1"`, 1), strings.Replace(good, `32766`, `100`, 1)} {
		if err := validateRoutingRules(bad); err == nil {
			t.Fatal(bad)
		}
	}
}
func TestConflictingConfigRejectedBeforeStaging(t *testing.T) {
	for _, path := range []string{"/etc/nftables.conf", "/etc/ufw/ufw.conf", "/etc/firewalld/firewalld.conf", "/etc/iptables/rules.v6"} {
		t.Run(path, func(t *testing.T) {
			h := fixture(t)
			if err := h.m.atomic(path, []byte("foreign"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := h.m.Enable(context.Background(), "host"); err == nil {
				t.Fatal("accepted conflicting config")
			}
			if _, err := os.Stat(h.m.path(statePath)); !os.IsNotExist(err) {
				t.Fatal("staged before validation")
			}
		})
	}
}

func TestProviderHostRoutesDoNotPreventAdmission(t *testing.T) {
	h := fixture(t)
	original := h.m.Run
	h.m.Run = func(ctx context.Context, name string, args []string) (process.Result, error) {
		if name == "ip" && strings.Contains(strings.Join(args, " "), "route show") {
			return process.Result{Stdout: []byte(`[{"dst":"169.254.169.254","dev":"eth0","protocol":"static"}]`)}, nil
		}
		return original(ctx, name, args)
	}
	if _, err := h.m.inspect(context.Background(), "host"); err != nil {
		t.Fatal(err)
	}
}

func TestVirtualUplinkAdmission(t *testing.T) {
	h := fixture(t)
	original := h.m.Run
	h.m.Run = func(ctx context.Context, name string, args []string) (process.Result, error) {
		if name == "ip" && strings.Join(args, " ") == "-j -d link show" {
			return process.Result{Stdout: []byte(`[{"ifname":"lo"},{"ifname":"eth0","linkinfo":{"info_kind":"veth"}}]`)}, nil
		}
		return original(ctx, name, args)
	}
	if _, err := h.m.inspect(context.Background(), "host"); err != nil {
		t.Fatal(err)
	}
}

func TestActualDockerBackendForms(t *testing.T) {
	for _, s := range []string{``, `null`, `"iptables"`, `{"Driver":"iptables"}`} {
		if !dockerIPTablesBackend([]byte(s)) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{`"nftables"`, `{"Driver":"nftables"}`, `{}`, `42`} {
		if dockerIPTablesBackend([]byte(s)) {
			t.Fatal(s)
		}
	}
}

func TestBuildkitBaselineOwnership(t *testing.T) {
	host := strings.Repeat("a", 32)
	text := "*mangle\n:PREROUTING ACCEPT [0:0]\n:FORWARD ACCEPT [0:0]\n" + strings.Join(buildkitGuards(host), "\n") + "\nCOMMIT\n"
	if err := validateHostBaseline(text, host); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.ReplaceAll(text, host, strings.Repeat("b", 32)), strings.Replace(text, "-j DROP", "-j ACCEPT", 1), strings.Replace(text, "-o br-+", "-o eth0", 1)} {
		if err := validateHostBaseline(bad, host); err == nil {
			t.Fatal("foreign mangle policy admitted")
		}
	}
}

func TestAdmissionAutomaticPatternsAndOverrides(t *testing.T) {
	h := fixture(t)
	if err := h.m.atomic("/etc/docker/daemon.json", []byte(`{"bridge":"none","log-driver":"local"}`), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := h.m.inspect(context.Background(), "host")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Bridges, ",") != "br-+,vamos-build0" {
		t.Fatal(p)
	}
	for _, config := range []string{`{"bridge":"docker0"}`, `{"iptables":false}`, `{"allow-direct-routing":true}`} {
		if err := h.m.atomic("/etc/docker/daemon.json", []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := h.m.inspect(context.Background(), "host"); err == nil {
			t.Fatal(config)
		}
	}
}

func TestContainerAdmissionAndLoopbackRawInventory(t *testing.T) {
	host := strings.Repeat("a", 32)
	for _, tc := range []struct {
		name, mode, labels string
		privileged, want   bool
	}{
		{"app", "vamos-test", `"vamos.host":"` + host + `","vamos.app":"test"`, false, true},
		{"foreign", "vamos-test", ``, false, false},
		{"host-app", "host", `"vamos.host":"` + host + `","vamos.app":"test"`, false, false},
		{"privileged", "vamos-test", `"vamos.host":"` + host + `","vamos.app":"test"`, true, false},
		{"vamos-caddy", "host", `"org.vamos.caddy.host":"` + host + `"`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := fixture(t)
			h.m.Run = func(_ context.Context, _ string, args []string) (process.Result, error) {
				out := strings.Repeat("b", 64)
				if args[1] == "inspect" {
					out = fmt.Sprintf(`[{"Name":%q,"Config":{"Labels":{%s}},"HostConfig":{"NetworkMode":%q,"Privileged":%t},"NetworkSettings":{"Ports":{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"32123"}]}}}]`, "/"+tc.name, tc.labels, tc.mode, tc.privileged)
				}
				return process.Result{Stdout: []byte(out)}, nil
			}
			raw := map[string]bool{}
			err := h.m.inspectContainers(context.Background(), host, map[string]string{"vamos-test": "test"}, raw)
			if (err == nil) != tc.want {
				t.Fatal(err)
			}
			if tc.name == "app" && !raw["-A PREROUTING -d 127.0.0.1/32 ! -i lo -p tcp -m tcp --dport 32123 -j DROP"] {
				t.Fatal(raw)
			}
		})
	}
}

func TestForeignNATAndDockerNamedMangleRejected(t *testing.T) {
	for _, rule := range []string{
		"*nat\n:PREROUTING ACCEPT [0:0]\n-A PREROUTING -s 10.0.0.0/8 -j DOCKER\nCOMMIT\n",
		"*mangle\n:DOCKER - [0:0]\n-A DOCKER -j ACCEPT\nCOMMIT\n",
	} {
		if err := validateBaseline(rule); err == nil {
			t.Fatal(rule)
		}
	}
}

type missingCommand struct{}

func (missingCommand) Error() string { return "exit status 127" }
func (missingCommand) ExitCode() int { return 127 }

func TestNFTLibraryFallbackForEnvWrappedMissingCLI(t *testing.T) {
	h := fixture(t)
	h.m.Run = func(_ context.Context, name string, _ []string) (process.Result, error) {
		if name == "nft" {
			return process.Result{}, fmt.Errorf("run env: %w", missingCommand{})
		}
		if name != "python3" {
			t.Fatal(name)
		}
		return process.Result{Stdout: []byte(`{"nftables":[]}`)}, nil
	}
	if _, err := h.m.nft(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

func TestNFTLibraryFallbackOnlyForMissingCLI(t *testing.T) {
	h := fixture(t)
	calls := 0
	h.m.Run = func(_ context.Context, name string, args []string) (process.Result, error) {
		calls++
		if name == "nft" {
			return process.Result{}, &exec.Error{Name: "nft", Err: exec.ErrNotFound}
		}
		if name != "python3" || args[0] != "-c" || args[1] != nftLibraryInspect || args[2] != "16" || args[3] != "list ruleset" {
			t.Fatal(name, args)
		}
		return process.Result{Stdout: []byte(`{"nftables":[]}`)}, nil
	}
	if out, err := h.m.nft(context.Background(), true); err != nil || out != `{"nftables":[]}` || calls != 2 {
		t.Fatal(out, err, calls)
	}
	h.m.Run = func(_ context.Context, name string, _ []string) (process.Result, error) {
		if name != "nft" {
			t.Fatal("retried failed inspection with another backend")
		}
		return process.Result{}, errors.New("inspection failed")
	}
	if _, err := h.m.nft(context.Background(), true); err == nil {
		t.Fatal("failed open")
	}
}
