# Read-only commands use the matching uploaded executor without installing it.
set -eu
[ "$(id -u)" = 0 ] || { printf '%s\n' 'deployment requires root' >&2; exit 1; }
stage=$(mktemp -d /run/soar-read.XXXXXXXX)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
tar -xf - -C "$stage"
(cd "$stage" && sha256sum --status -c checksum)
chmod 700 "$stage/executor"
cd "$stage"
./executor internal-app-executor < request.json
