package firewall

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The real generated shell runs against a tiny deterministic iptables model.
// This is not a kernel/netfilter test; VM tests must validate kernel semantics.
func TestIPTablesHelper(t *testing.T) {
	if os.Getenv("VAMOS_FIREWALL_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	family := filepath.Base(args[0])
	args = args[1:]
	table := "filter"
	for len(args) > 1 && (args[0] == "-w" || args[0] == "-t") {
		if args[0] == "-t" {
			table = args[1]
		}
		args = args[2:]
	}
	if table != "filter" {
		family += "-" + table
	}
	file := filepath.Join(os.Getenv("VAMOS_FIREWALL_DB"), family+".json")
	chains := map[string][][]string{"INPUT": {}, "FORWARD": {}, "OUTPUT": {}}
	if data, e := os.ReadFile(file); e == nil {
		if e = json.Unmarshal(data, &chains); e != nil {
			os.Exit(2)
		}
	}
	exit := func(code int) {
		if code == 0 {
			data, _ := json.Marshal(chains)
			if os.WriteFile(file, data, 0600) != nil {
				os.Exit(2)
			}
		}
		os.Exit(code)
	}
	op := args[0]
	chain := ""
	if len(args) > 1 {
		chain = args[1]
	}
	rules, exists := chains[chain]
	switch op {
	case "-S":
		printChain := func(c string) {
			switch c {
			case "INPUT", "FORWARD", "OUTPUT":
				fmt.Printf("-P %s ACCEPT\n", c)
			default:
				fmt.Printf("-N %s\n", c)
			}
			for _, r := range chains[c] {
				r = append([]string(nil), r...)
				for i := range r {
					if i > 0 && r[i-1] == "--comment" {
						r[i] = strconv.Quote(r[i])
					}
				}
				fmt.Printf("-A %s %s\n", c, strings.Join(r, " "))
			}
		}
		if chain != "" {
			if !exists {
				exit(1)
			}
			printChain(chain)
		} else {
			for c := range chains {
				printChain(c)
			}
		}
	case "-N":
		if exists {
			exit(1)
		}
		chains[chain] = [][]string{}
	case "-A", "-I":
		if !exists {
			exit(1)
		}
		r := args[2:]
		if op == "-I" {
			r = r[1:]
			chains[chain] = append([][]string{r}, rules...)
		} else {
			chains[chain] = append(rules, r)
		}
	case "-C", "-D":
		found := -1
		for i, r := range rules {
			if reflect.DeepEqual(r, args[2:]) {
				found = i
				break
			}
		}
		if found < 0 {
			exit(1)
		}
		if op == "-D" {
			chains[chain] = append(rules[:found], rules[found+1:]...)
		}
	case "-F":
		if !exists {
			exit(1)
		}
		chains[chain] = [][]string{}
	case "-X":
		if !exists || len(rules) > 0 {
			exit(1)
		}
		for _, rs := range chains {
			for _, r := range rs {
				if len(r) > 1 && r[len(r)-1] == chain {
					exit(1)
				}
			}
		}
		delete(chains, chain)
	default:
		exit(2)
	}
	exit(0)
}

func TestGeneratedShellLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executes server-side POSIX shell and directory fsync")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	db := filepath.Join(root, "db")
	for _, d := range []string{bin, db, filepath.Join(root, "var/lib/vamos")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"iptables", "ip6tables"} {
		wrapper := fmt.Sprintf("#!/bin/sh\nexec '%s' -test.run='^TestIPTablesHelper$' -- \"$0\" \"$@\"\n", exe)
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(wrapper), 0700); err != nil {
			t.Fatal(err)
		}
	}
	s := state{Version: 1, HostID: strings.Repeat("a", 32), Token: strings.Repeat("b", 64), Connection: "203.0.113.1 40000 10.0.0.1 2222", Deadline: time.Now().Add(time.Minute), Plan: plan{SSHPorts: []int{2222}, Bridges: []string{"br-+", "vamos-build0"}}}
	m := Manager{Root: root}
	if err = m.save(s); err != nil {
		t.Fatal(err)
	}
	body := script(s)
	body = strings.ReplaceAll(body, "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "PATH="+bin+":/usr/sbin:/usr/bin:/sbin:/bin")
	body = strings.ReplaceAll(body, "/run/lock", filepath.Join(root, "run/lock"))
	body = strings.ReplaceAll(body, "/var/lib/vamos", filepath.Join(root, "var/lib/vamos"))
	// Linux sync -f has no equivalent on macOS; atomic persistence is separately
	// exercised by manager tests. Do not touch the real host filesystem.
	body = strings.ReplaceAll(body, "sync -f "+filepath.Join(root, "var/lib/vamos"), ":")
	scriptFile := filepath.Join(root, "firewall.sh")
	if err = os.WriteFile(scriptFile, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(action string, wantSuccess bool) {
		t.Helper()
		cmd := exec.Command("/bin/sh", scriptFile, action, "--locked")
		cmd.Env = append(os.Environ(), "VAMOS_FIREWALL_HELPER=1", "VAMOS_FIREWALL_DB="+db, "GORACE=atexit_sleep_ms=0")
		out, e := cmd.CombinedOutput()
		if (e == nil) != wantSuccess {
			t.Fatalf("%s: %v %s", action, e, out)
		}
	}
	run("install", true)
	// A first build after enable adds its own DROP-only mangle guards ahead of
	// our hook; status, restart guards, boot and removal must preserve them.
	for _, family := range []string{"iptables", "ip6tables"} {
		file := filepath.Join(db, family+"-mangle.json")
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var chains map[string][][]string
		if err := json.Unmarshal(data, &chains); err != nil {
			t.Fatal(err)
		}
		for _, guard := range buildkitGuards(s.HostID) {
			tokens := strings.Fields(strings.ReplaceAll(guard, `"`, ""))
			if tokens[1] == "FORWARD" {
				chains["FORWARD"] = append([][]string{tokens[2:]}, chains["FORWARD"]...)
			}
		}
		data, _ = json.Marshal(chains)
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("verify", true)
	// Unconfirmed reboot must remove both families, not reapply them.
	run("boot", true)
	if _, err = os.Stat(m.path(statePath)); !os.IsNotExist(err) {
		t.Fatal("unconfirmed reboot retained state")
	}
	if err = m.save(s); err != nil {
		t.Fatal(err)
	}
	if err = m.atomic(confirmedPath, []byte(s.Token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("boot", true)
	run("guard", true)
	run("rollback", true)
	run("verify", true)
	// Inject a foreign rule into one owned chain. Neither family may be removed.
	file := filepath.Join(db, "ip6tables.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var chains map[string][][]string
	if err = json.Unmarshal(data, &chains); err != nil {
		t.Fatal(err)
	}
	chains[inputChain] = append(chains[inputChain], []string{"-j", "ACCEPT"})
	modified, _ := json.Marshal(chains)
	if err = os.WriteFile(file, modified, 0600); err != nil {
		t.Fatal(err)
	}
	run("remove", false)
	if _, err = os.Stat(m.path(statePath)); err != nil {
		t.Fatal("drift lost identity")
	}
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	run("remove", true)
	for _, tool := range []string{"iptables", "ip6tables", "iptables-mangle", "ip6tables-mangle"} {
		data, err := os.ReadFile(filepath.Join(db, tool+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "VAMOS-FW-") {
			t.Fatal("owned rules survived removal")
		}
		if strings.HasSuffix(tool, "-mangle") && !strings.Contains(string(data), "vamos-buildkit:") {
			t.Fatal("BuildKit guards removed")
		}
	}
}
