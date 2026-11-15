# Fixed app worker transport; metadata and build context are archive entries,
# never interpolated shell source. The worker owns inputs once launched.
set -eu
[ "$(id -u)" = 0 ] || { printf '%s\n' 'deployment requires root' >&2; exit 1; }
stage=$(mktemp -d /run/vamos-stage.XXXXXXXX)
execdir=$(mktemp -d /var/lib/.vamos-exec.XXXXXXXX)
trap 'rm -rf "$stage" "$execdir"' EXIT HUP INT TERM
tar -xf - -C "$stage"
[ "$(stat -c %a "$stage")" = 700 ] || exit 1
for file in request.json checksum; do
  [ -f "$stage/$file" ] && [ ! -L "$stage/$file" ] && [ "$(stat -c %a "$stage/$file")" = 600 ] || exit 1
done
for private in secret.value repository.credentials; do
  if [ -e "$stage/$private" ] || [ -L "$stage/$private" ]; then
    [ -f "$stage/$private" ] && [ ! -L "$stage/$private" ] && [ "$(stat -c %a "$stage/$private")" = 600 ] || exit 1
  fi
done
[ -f "$stage/executor" ] && [ ! -L "$stage/executor" ] || exit 1
(cd "$stage" && sha256sum --status -c checksum)
mv "$stage/executor" "$execdir/executor"
chmod 700 "$execdir/executor"
trap - EXIT HUP INT TERM
set +e
systemd-run --quiet --collect --wait --pipe --unit=vamos-host-operation \
  --property=RuntimeMaxSec=21min --property=KillMode=control-group \
  /bin/sh -c 'stage=$1; execdir=$2; trap '\''rm -rf "$stage" "$execdir"'\'' EXIT; cd "$stage"; "$execdir/executor" internal-app-executor < request.json' sh "$stage" "$execdir"
status=$?
rm -rf "$stage" "$execdir"
exit "$status"
