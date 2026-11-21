package firewall

import (
	"fmt"
	"strings"
)

func managedChains(table string) []string {
	if table == "mangle" {
		return []string{forwardChain}
	}
	return []string{inputChain, forwardChain}
}

// Render the reference listing with the host's own xtables backend in an
// empty network namespace. -S normalizes addresses, match modules and option
// order; comparing its output to our input tokens would reject valid rules.
func verifyOrder(family, table string, rules []string) string {
	var b strings.Builder
	tool := family + " -w 10 -t " + table
	b.WriteString("expected=$(unshare --net /bin/sh -e <<'EOF'\n")
	var list strings.Builder
	for _, chain := range managedChains(table) {
		fmt.Fprintf(&b, "%s -N %s\n", tool, chain)
		for _, r := range rules {
			if strings.HasPrefix(r, "-A "+chain+" ") {
				fmt.Fprintf(&b, "%s %s\n", tool, r)
			}
		}
		fmt.Fprintf(&list, "%s -S %s || exit 1\n", tool, chain)
	}
	fmt.Fprintf(&b, "%sEOF\n)\nactual=$(\n%s)\n[ \"$expected\" = \"$actual\" ] || exit 1\n", list.String(), list.String())
	return b.String()
}
