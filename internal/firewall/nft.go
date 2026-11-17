package firewall

import (
	"encoding/json"
	"fmt"
	"strings"
)

// iptables-nft is supported, but native nftables objects must not be hidden by
// iptables-save's compatibility view. Compare chain/rule inventories and base
// chain hooks/priorities; reject sets, maps, flowtables and additional objects.
// Rule semantics are then restricted by validateBaseline and Docker inventory.
func validateNFT(data string, saves map[string]string) error {
	expected := map[string]int{}
	policies := map[string]string{}
	for family, text := range saves {
		table := ""
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "*") {
				table = line[1:]
				continue
			}
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			if strings.HasPrefix(f[0], ":") {
				key := family + "/" + table + "/" + f[0][1:]
				expected[key] = 0
				policies[key] = strings.ToLower(f[1])
			}
			if f[0] == "-A" {
				expected[family+"/"+table+"/"+f[1]]++
			}
		}
	}
	var doc struct {
		NFTables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		return err
	}
	actual := map[string]int{}
	for _, obj := range doc.NFTables {
		if len(obj) != 1 {
			return fmt.Errorf("unexpected nftables object")
		}
		for typ, raw := range obj {
			if typ == "metainfo" {
				continue
			}
			var x struct {
				Family, Table, Name, Chain, Type, Hook, Policy string
				Prio                                           int
				Flags                                          []string
			}
			if err := json.Unmarshal(raw, &x); err != nil {
				return err
			}
			if x.Family != "ip" && x.Family != "ip6" {
				return fmt.Errorf("native nftables family %s", x.Family)
			}
			switch typ {
			case "table":
				if x.Name != "filter" && x.Name != "nat" && x.Name != "raw" && x.Name != "mangle" {
					return fmt.Errorf("native nftables table %s", x.Name)
				}
			case "chain":
				key := x.Family + "/" + x.Table + "/" + x.Name
				policy, ok := policies[key]
				if !ok {
					return fmt.Errorf("hidden nftables chain %s", key)
				}
				actual[key] += 0
				if len(x.Flags) > 0 {
					return fmt.Errorf("native nftables chain flags")
				}
				if policy == "-" {
					if x.Hook != "" || x.Type != "" {
						return fmt.Errorf("native base chain")
					}
					continue
				}
				priority := 0
				kind := "filter"
				switch x.Table {
				case "nat":
					kind = "nat"
					priority = -100
					if x.Name == "POSTROUTING" || x.Name == "INPUT" {
						priority = 100
					}
				case "raw":
					priority = -300
				case "mangle":
					priority = -150
				}
				// iptables-nft uses route chains for mangle OUTPUT.
				if x.Table == "mangle" && x.Name == "OUTPUT" {
					kind = "route"
				}
				if x.Type != kind || x.Hook != strings.ToLower(x.Name) || x.Prio != priority || x.Policy != policy {
					return fmt.Errorf("native nftables hook/policy %s", key)
				}
			case "rule":
				actual[x.Family+"/"+x.Table+"/"+x.Chain]++
			default:
				return fmt.Errorf("native nftables %s unsupported", typ)
			}
		}
	}
	for key := range actual {
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("hidden nftables chain %s", key)
		}
	}
	for key, n := range expected {
		got, ok := actual[key]
		// The iptables compatibility view synthesizes unused ACCEPT builtins.
		// Their absence has the same effect as an empty accept-policy chain;
		// never extend this exception to user chains, rules or DROP policies.
		if !ok && n == 0 && policies[key] == "accept" {
			continue
		}
		if !ok || got != n {
			return fmt.Errorf("iptables/nftables rule inventory mismatch at %s", key)
		}
	}
	return nil
}
