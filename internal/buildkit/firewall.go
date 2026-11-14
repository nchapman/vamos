package buildkit

import (
	"context"
	"reflect"
	"strings"
)

type guard struct {
	chain string
	args  []string
}

func (b *build) guards() []guard {
	comment := "soar-buildkit:" + b.s.Host
	return []guard{
		{"PREROUTING", []string{"-i", bridge, "-m", "addrtype", "--dst-type", "LOCAL", "-m", "comment", "--comment", comment, "-j", "DROP"}},
		{"FORWARD", []string{"-i", bridge, "-o", "br-+", "-m", "comment", "--comment", comment, "-j", "DROP"}},
		{"FORWARD", []string{"-i", bridge, "-o", "docker0", "-m", "comment", "--comment", comment, "-j", "DROP"}},
		{"FORWARD", []string{"-i", bridge, "-o", bridge, "-m", "comment", "--comment", comment, "-j", "DROP"}},
	}
}

// iptables -S quotes comments. Our generated tokens contain no shell syntax;
// splitting is only used to compare exact owned rules, never to execute output.
func ruleTokens(line string) []string {
	t := strings.Fields(line)
	for i := range t {
		t[i] = strings.Trim(t[i], `"`)
	}
	return t
}
func (b *build) rules(ctx context.Context, exe, chain string) ([][]string, error) {
	r, err := b.command(ctx, exe, []string{"-w", "5", "-t", "mangle", "-S", chain}, nil)
	if err != nil {
		return nil, errInfra
	}
	var rules [][]string
	policy := false
	for _, line := range strings.Split(strings.TrimSpace(string(r.Stdout)), "\n") {
		t := ruleTokens(line)
		if len(t) == 3 && t[0] == "-P" && t[1] == chain && (t[2] == "ACCEPT" || t[2] == "DROP") {
			if policy {
				return nil, errInfra
			}
			policy = true
			continue
		}
		if len(t) < 3 || t[0] != "-A" || t[1] != chain {
			return nil, errInfra
		}
		rules = append(rules, t[2:])
	}
	if !policy {
		return nil, errInfra
	}
	return rules, nil
}
func (b *build) firewall(ctx context.Context) error {
	// Caller has stopped the verified container before even inspecting guards.
	for _, exe := range []string{"/usr/sbin/iptables", "/usr/sbin/ip6tables"} {
		for _, chain := range []string{"PREROUTING", "FORWARD"} {
			var desired [][]string
			for _, g := range b.guards() {
				if g.chain == chain {
					desired = append(desired, g.args)
				}
			}
			current, err := b.rules(ctx, exe, chain)
			if err != nil {
				return err
			}
			good := len(current) >= len(desired)
			if good {
				for i := range desired {
					if !reflect.DeepEqual(current[i], desired[i]) {
						good = false
					}
				}
			}
			if !good {
				// Move only exact owned rules; unknown and near-matching rules are left
				// untouched. Insertion at 1 is mandatory, not just iptables -C success.
				for _, r := range current {
					for _, want := range desired {
						if reflect.DeepEqual(r, want) {
							args := append([]string{"-w", "5", "-t", "mangle", "-D", chain}, want...)
							if _, err = b.command(ctx, exe, args, nil); err != nil {
								return errInfra
							}
						}
					}
				}
				for i := len(desired) - 1; i >= 0; i-- {
					args := append([]string{"-w", "5", "-t", "mangle", "-I", chain, "1"}, desired[i]...)
					if _, err = b.command(ctx, exe, args, nil); err != nil {
						return errInfra
					}
				}
			}
			actual, err := b.rules(ctx, exe, chain)
			if err != nil || len(actual) < len(desired) {
				return errInfra
			}
			for i := range desired {
				if !reflect.DeepEqual(actual[i], desired[i]) {
					return errInfra
				}
			}
		}
	}
	return nil
}
