package firewall

import (
	"fmt"
	"strings"
)

const scriptPath = "/usr/local/lib/vamos/firewall.sh"
const servicePath = "/etc/systemd/system/vamos-firewall.service"
const timerPath = "/etc/systemd/system/vamos-firewall-rollback.timer"
const rollbackPath = "/etc/systemd/system/vamos-firewall-rollback.service"
const dropinPath = "/etc/systemd/system/docker.service.d/50-vamos-firewall.conf"
const retiredPath = "/var/lib/vamos/firewall.retired.json"
const statePath = "/var/lib/vamos/firewall.json"
const confirmedPath = "/var/lib/vamos/firewall.confirmed"
const lockPath = "/run/lock/vamos-firewall.lock"

const service = `[Unit]
Description=Vamos dedicated-host firewall
DefaultDependencies=no
After=local-fs.target
Before=network-pre.target docker.service shutdown.target
Wants=network-pre.target
Conflicts=shutdown.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/lib/vamos/firewall.sh boot

[Install]
WantedBy=multi-user.target
`
const timer = `[Unit]
Description=Rollback unconfirmed Vamos firewall

[Timer]
OnActiveSec=300s
AccuracySec=1s
Unit=vamos-firewall-rollback.service

[Install]
WantedBy=timers.target
`
const rollbackService = `[Unit]
Description=Rollback unconfirmed Vamos firewall independently of SSH
After=local-fs.target

[Service]
Type=oneshot
ExecStart=/usr/local/lib/vamos/firewall.sh rollback
Restart=on-failure
RestartSec=10s
`
const dropin = `[Unit]
Requires=vamos-firewall.service
After=vamos-firewall.service

[Service]
ExecStartPre=/usr/local/lib/vamos/firewall.sh guard
`

func artifacts(s state) map[string]string {
	return map[string]string{scriptPath: script(s), servicePath: service, timerPath: timer, rollbackPath: rollbackService, dropinPath: dropin}
}

// All generated data is validated before this function. The rollback executable
// is self-contained: no vamos binary, Docker daemon, caller, or JSON tool needed.
func script(s state) string {
	var b strings.Builder
	fmt.Fprintf(&b, `#!/bin/sh
# Owned by vamos firewall; generation %s
set -eu
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
TOKEN='%s'
STATE=/var/lib/vamos/firewall.json
CONFIRMED=/var/lib/vamos/firewall.confirmed
mkdir -p /run/lock
if [ "${2:-}" != '--locked' ]; then
    exec 9>/run/lock/vamos-firewall.lock
    flock -x 9
fi
confirmed() { [ -f "$CONFIRMED" ] && [ "$(cat "$CONFIRMED")" = "$TOKEN" ]; }
# State and script must have the same durable identity. Never touch another generation.
identity() { [ -f "$STATE" ] && grep -Fq '"token": "%s"' "$STATE"; }
`, s.Token, s.Token, s.Token)
	for _, family := range []string{"iptables", "ip6tables"} {
		tag := "v4"
		if family == "ip6tables" {
			tag = "v6"
		}
		rules := s.Plan.rules(family)
		fmt.Fprintf(&b, "subset_%s() {\n    tool=%s\n    snapshot=$($tool -w 10 -S)\n", tag, family)
		// Verify rule count against the set of expected rules that actually exist.
		for _, chain := range []string{inputChain, forwardChain} {
			fmt.Fprintf(&b, "    if printf '%%s\\n' \"$snapshot\" | grep -q '^-N %s$'; then\n        listing=$($tool -w 10 -S %s)\n        actual=$(printf '%%s\\n' \"$listing\" | grep -c '^-A ' || :)\n        known=0\n", chain, chain)
			for _, r := range rules {
				if strings.HasPrefix(r, "-A "+chain+" ") {
					fmt.Fprintf(&b, "        if $tool -w 10 %s >/dev/null 2>&1; then known=$((known+1)); fi\n", strings.Replace(r, "-A ", "-C ", 1))
				}
			}
			b.WriteString("        [ \"$actual\" -eq \"$known\" ] || { echo 'foreign owned-chain contents' >&2; return 1; }\n    fi\n")
		}
		// Reject unexpected references, including goto and other chains. Builtin
		// hook ordering is separately checked during full verification.
		fmt.Fprintf(&b, `    refs=$(printf '%%s\n' "$snapshot" | grep -E -- ' (-j|-g) VAMOS-FW-' || :)
    for hook in '-A INPUT -j VAMOS-FW-IN' '-A FORWARD -j VAMOS-FW-FWD' '-A DOCKER-USER -j VAMOS-FW-FWD'; do
        count=$(printf '%%s\n' "$refs" | grep -Fxc -- "$hook" || :)
        [ "$count" -le 1 ] || { echo 'duplicate owned hook' >&2; return 1; }
    done
    while IFS= read -r line; do
        case "$line" in
            ''|'-A INPUT -j VAMOS-FW-IN'|'-A FORWARD -j VAMOS-FW-FWD'|'-A DOCKER-USER -j VAMOS-FW-FWD') ;;
            *) echo 'foreign reference to owned firewall' >&2; return 1 ;;
        esac
    done <<EOF
$refs
EOF
}
verify_%s() {
    subset_%s
`, tag, tag)
		for _, r := range rules {
			fmt.Fprintf(&b, "    %s -w 10 %s\n", family, strings.Replace(r, "-A ", "-C ", 1))
		}
		for _, hook := range []string{"INPUT -j " + inputChain, "FORWARD -j " + forwardChain, "DOCKER-USER -j " + forwardChain} {
			fmt.Fprintf(&b, "    %s -w 10 -C %s\n", family, hook)
		}
		fmt.Fprintf(&b, `    [ "$(%s -w 10 -S INPUT | grep '^-A ' | head -n 1)" = '-A INPUT -j VAMOS-FW-IN' ]
    [ "$(%s -w 10 -S DOCKER-USER | grep '^-A ' | head -n 1)" = '-A DOCKER-USER -j VAMOS-FW-FWD' ]
    first=$($tool -w 10 -S FORWARD | grep '^-A ' | head -n 1)
    case "$first" in '-A FORWARD -j VAMOS-FW-FWD'|'-A FORWARD -j DOCKER-USER') ;; *) return 1 ;; esac
    output=$($tool -w 10 -S OUTPUT)
    [ "$output" = '-P OUTPUT ACCEPT' ]
    input=$($tool -w 10 -S INPUT)
    [ "$(printf '%%s\n' "$input" | head -n 1)" = '-P INPUT ACCEPT' ]
    [ "$(printf '%%s\n' "$input" | grep -c '^-A ')" -eq 1 ]
    user=$($tool -w 10 -S DOCKER-USER)
    foreign=$(printf '%%s\n' "$user" | grep '^-A ' | grep -Ev '^-A DOCKER-USER -j (VAMOS-FW-FWD|RETURN)$' || :)
    [ -z "$foreign" ]
    # Docker may insert its own FORWARD jumps ahead of our pre-daemon hook;
    # those must traverse DOCKER-USER before accepting published traffic.
    foreign=$(%s -w 10 -S FORWARD | grep '^-A ' | grep -Ev '^-A FORWARD -j (VAMOS-FW-FWD|DOCKER-USER|DOCKER-FORWARD|DOCKER-ISOLATION-STAGE-1)$' || :)
    [ -z "$foreign" ]
}
install_%s() {
`, family, family, family, tag)
		fmt.Fprintf(&b, "    %s -w 10 -N %s\n    %s -w 10 -N %s\n", family, inputChain, family, forwardChain)
		for _, r := range rules {
			fmt.Fprintf(&b, "    %s -w 10 %s\n", family, r)
		}
		fmt.Fprintf(&b, `    %s -w 10 -S DOCKER-USER >/dev/null 2>&1 || %s -w 10 -N DOCKER-USER
    %s -w 10 -I DOCKER-USER 1 -j VAMOS-FW-FWD
    %s -w 10 -I FORWARD 1 -j VAMOS-FW-FWD
    %s -w 10 -I INPUT 1 -j VAMOS-FW-IN
}
remove_%s() {
`, family, family, family, family, family, tag)
		for _, hook := range []string{"INPUT -j " + inputChain, "FORWARD -j " + forwardChain, "DOCKER-USER -j " + forwardChain} {
			fmt.Fprintf(&b, "    while %s -w 10 -C %s >/dev/null 2>&1; do %s -w 10 -D %s; done\n", family, hook, family, hook)
		}
		for _, chain := range []string{inputChain, forwardChain} {
			fmt.Fprintf(&b, "    if %s -w 10 -S %s >/dev/null 2>&1; then %s -w 10 -F %s; %s -w 10 -X %s; fi\n", family, chain, family, chain, family, chain)
		}
		b.WriteString("}\n")
	}
	b.WriteString(earlyScript(s))
	b.WriteString(`remove_owned() {
    # Validate BOTH families before mutating either; on failure retain identity
    # and let systemd retry. Partial installs are valid subsets, not foreign rules.
    subset_v4
    subset_v6
    subset_v4_early
    subset_v6_early
    remove_v4
    remove_v6
    remove_v4_early
    remove_v6_early
    for tool in iptables ip6tables; do
        for table in filter mangle; do
            remaining=$($tool -t "$table" -w 10 -S)
            if printf '%s\n' "$remaining" | grep -q 'VAMOS-FW-'; then echo 'owned rules remain after rollback' >&2; return 1; fi
        done
    done
    rm -f "$CONFIRMED"
    mv "$STATE" /var/lib/vamos/firewall.retired.json
    sync -f /var/lib/vamos
}
case "${1:-}" in
    verify)
        identity
        verify_v4
        verify_v6
        verify_v4_early
        verify_v6_early
        ;;
    install)
        identity
        install_v4_early
        install_v6_early
        install_v4
        install_v6
        verify_v4
        verify_v6
        verify_v4_early
        verify_v6_early
        ;;
    rollback)
        # A late timer cannot undo a confirmed generation.
        if confirmed; then exit 0; fi
        if ! [ -e "$STATE" ]; then exit 0; fi
        identity
        remove_owned
        ;;
    remove)
        identity
        verify_v4
        verify_v6
        verify_v4_early
        verify_v6_early
        remove_owned
        ;;
    boot)
        if ! [ -e "$STATE" ]; then exit 0; fi
        identity
        if ! confirmed; then remove_owned; exit 0; fi
        # At boot chains are absent. Existing chains must verify; never flush
        # foreign or drifted state to repair it.
        if iptables -w 10 -S VAMOS-FW-IN >/dev/null 2>&1 || ip6tables -w 10 -S VAMOS-FW-IN >/dev/null 2>&1; then
            verify_v4
            verify_v6
            verify_v4_early
            verify_v6_early
        else
            install_v4_early
            install_v6_early
            install_v4
            install_v6
            verify_v4
            verify_v6
            verify_v4_early
            verify_v6_early
        fi
        ;;
    guard)
        if ! [ -e "$STATE" ]; then exit 0; fi
        identity
        verify_v4
        verify_v6
        verify_v4_early
        verify_v6_early
        ;;
    *) echo 'invalid firewall action' >&2; exit 1 ;;
esac
`)
	return b.String()
}

// A mangle/FORWARD gate precedes filter/FORWARD regardless of Docker's temporary
// jump reordering during startup. DNAT has already happened in PREROUTING. This
// gate only DROPs or RETURNs, so it cannot bypass Docker's later isolation rules.
func earlyScript(s state) string {
	var b strings.Builder
	for _, family := range []string{"iptables", "ip6tables"} {
		tag := "v4_early"
		if family == "ip6tables" {
			tag = "v6_early"
		}
		rules := []string{}
		for _, r := range s.Plan.rules(family) {
			if strings.HasPrefix(r, "-A "+forwardChain+" ") {
				rules = append(rules, r)
			}
		}
		fmt.Fprintf(&b, `subset_%s() {
    tool='%s -t mangle'
    snapshot=$($tool -w 10 -S)
    if printf '%%s\n' "$snapshot" | grep -q '^-N VAMOS-FW-FWD$'; then
        listing=$($tool -w 10 -S VAMOS-FW-FWD)
        actual=$(printf '%%s\n' "$listing" | grep -c '^-A ' || :)
        known=0
`, tag, family)
		for _, r := range rules {
			fmt.Fprintf(&b, "        if $tool -w 10 %s >/dev/null 2>&1; then known=$((known+1)); fi\n", strings.Replace(r, "-A ", "-C ", 1))
		}
		b.WriteString(`        [ "$actual" -eq "$known" ] || { echo 'foreign early-chain contents' >&2; return 1; }
    fi
    refs=$(printf '%s\n' "$snapshot" | grep -E -- ' (-j|-g) VAMOS-FW-' || :)
    case "$refs" in ''|'-A FORWARD -j VAMOS-FW-FWD') ;; *) echo 'foreign early-chain reference' >&2; return 1 ;; esac
}
`)
		fmt.Fprintf(&b, "verify_%s() {\n    subset_%s\n", tag, tag)
		for _, r := range rules {
			fmt.Fprintf(&b, "    $tool -w 10 %s\n", strings.Replace(r, "-A ", "-C ", 1))
		}
		b.WriteString("    forward=$($tool -w 10 -S FORWARD)\n")
		// BuildKit may insert its DROP-only guards before our gate after enable.
		// Remove only exact host-owned guards from the verification view.
		for _, guard := range buildkitGuards(s.HostID) {
			if strings.HasPrefix(guard, "-A FORWARD ") {
				fmt.Fprintf(&b, "    forward=$(printf '%%s\\n' \"$forward\" | grep -Fvx -- '%s' || :)\n", guard)
			}
		}
		b.WriteString(`    [ "$forward" = '-P FORWARD ACCEPT
-A FORWARD -j VAMOS-FW-FWD' ]
}
`)
		fmt.Fprintf(&b, "install_%s() {\n    tool='%s -t mangle'\n    $tool -w 10 -N VAMOS-FW-FWD\n", tag, family)
		for _, r := range rules {
			fmt.Fprintf(&b, "    $tool -w 10 %s\n", r)
		}
		b.WriteString("    $tool -w 10 -I FORWARD 1 -j VAMOS-FW-FWD\n}\n")
		fmt.Fprintf(&b, `remove_%s() {
    tool='%s -t mangle'
    snapshot=$($tool -w 10 -S)
    if printf '%%s\n' "$snapshot" | grep -q '^-A FORWARD -j VAMOS-FW-FWD$'; then $tool -w 10 -D FORWARD -j VAMOS-FW-FWD; fi
    if printf '%%s\n' "$snapshot" | grep -q '^-N VAMOS-FW-FWD$'; then $tool -w 10 -F VAMOS-FW-FWD; $tool -w 10 -X VAMOS-FW-FWD; fi
}
`, tag, family)
	}
	return b.String()
}
