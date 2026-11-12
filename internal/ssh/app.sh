# Fixed app worker transport; metadata and build context are archive entries,
# never interpolated shell source. The worker owns inputs once launched.
set -eu
[ "$(id -u)" = 0 ] || { printf '%s\n' 'deployment requires root' >&2; exit 1; }
stage=$(mktemp -d /run/soar-stage.XXXXXXXX)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
tar -xf - -C "$stage"
[ "$(stat -c %a "$stage")" = 700 ] || exit 1
for file in request.json checksum; do
  [ -f "$stage/$file" ] && [ ! -L "$stage/$file" ] && [ "$(stat -c %a "$stage/$file")" = 600 ] || exit 1
done
if [ -e "$stage/secret.value" ] || [ -L "$stage/secret.value" ]; then
  [ -f "$stage/secret.value" ] && [ ! -L "$stage/secret.value" ] && [ "$(stat -c %a "$stage/secret.value")" = 600 ] || exit 1
fi
[ -f "$stage/executor" ] && [ ! -L "$stage/executor" ] || exit 1
(cd "$stage" && sha256sum --status -c checksum)
chmod 700 "$stage/executor"
trap - EXIT HUP INT TERM
set +e
systemd-run --quiet --collect --wait --pipe --unit=soar-host-operation \
  --property=RuntimeMaxSec=21min --property=KillMode=control-group \
  /bin/sh -c 'stage=$1; trap '\''rm -rf "$stage"'\'' EXIT; cd "$stage"; ./executor internal-app-executor < request.json' sh "$stage"
status=$?
rm -rf "$stage"
exit "$status"
