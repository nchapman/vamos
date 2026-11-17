package ssh

import "strings"

// shellQuote quotes one POSIX shell word, including embedded single quotes.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// adminCommand leaves stdin exclusively for the collector, archive, or session.
// SSH_CONNECTION is passed as data, not shell source or a sudo-preserved env
// variable (sudo commonly filters it). Only this single variable is restored.
func adminCommand(command string) string {
	script := `if [ "$(id -u)" = 0 ]; then exec /bin/sh -c ` + shellQuote(command) + `; fi
if ! command -v sudo >/dev/null 2>&1; then
  echo 'vamos: administration requires root or passwordless sudo; sudo is missing' >&2; exit 126
fi
if ! sudo -n -- /bin/sh -c 'test "$(id -u)" = 0' </dev/null 2>/dev/null; then
  echo 'vamos: administration requires passwordless sudo -n; sudo denied or requires a password' >&2; exit 126
fi
exec sudo -n -- /bin/sh -c ` + shellQuote(`SSH_CONNECTION=$1; export SSH_CONNECTION; exec /bin/sh -c `+shellQuote(command)) + ` vamos-admin "${SSH_CONNECTION-}"`
	return "/bin/sh -c " + shellQuote(script)
}
