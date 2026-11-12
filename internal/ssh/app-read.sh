# Read-only commands use the matching uploaded executor without installing it.
set -eu
[ "$(id -u)" = 0 ] || { printf '%s\n' 'deployment requires root' >&2; exit 1; }
stage=$(mktemp -d /run/soar-read.XXXXXXXX)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
tar -xf - -C "$stage"
[ "$(stat -c %a "$stage")" = 700 ] || exit 1
for file in request.json checksum; do
  [ -f "$stage/$file" ] && [ ! -L "$stage/$file" ] && [ "$(stat -c %a "$stage/$file")" = 600 ] || exit 1
done
[ -f "$stage/executor" ] && [ ! -L "$stage/executor" ] || exit 1
[ ! -e "$stage/secret.value" ] && [ ! -L "$stage/secret.value" ] || exit 1
(cd "$stage" && sha256sum --status -c checksum)
chmod 700 "$stage/executor"
cd "$stage"
./executor internal-app-executor < request.json
