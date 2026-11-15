# Session setup has bounded metadata I/O and never allocates a terminal.
set -eu
[ "$(id -u)" = 0 ] || { printf '%s\n' 'sessions require root' >&2; exit 1; }
stage=$(mktemp -d /run/vamos-session-upload.XXXXXXXX)
execbin=/var/lib/.vamos-session-exec.__TOKEN__
trap 'rm -rf "$stage" "$execbin"' EXIT HUP INT TERM
tar -xf - -C "$stage"
[ "$(stat -c %a "$stage")" = 700 ] || exit 1
for file in request.json checksum; do
  [ -f "$stage/$file" ] && [ ! -L "$stage/$file" ] && [ "$(stat -c %a "$stage/$file")" = 600 ] || exit 1
done
[ -f "$stage/executor" ] && [ ! -L "$stage/executor" ] || exit 1
[ ! -e "$stage/secret.value" ] && [ ! -L "$stage/secret.value" ] || exit 1
(cd "$stage" && sha256sum --status -c checksum)
mv -f "$stage/executor" "$execbin"
touch "$execbin"
chmod 700 "$execbin"
cd "$stage"
"$execbin" internal-session-prepare __TOKEN__ < request.json
trap 'rm -rf "$stage"' EXIT HUP INT TERM
for old in $(find /var/lib -maxdepth 1 -name '.vamos-session-exec.*' -mmin +3 2>/dev/null); do
  [ -e "/run/vamos-sessions/${old##*.}" ] || rm -f "$old"
done
