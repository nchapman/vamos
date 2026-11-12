# Fixed app worker transport; metadata and build context are archive entries,
# never interpolated shell source. The worker owns inputs once launched.
set -eu
[ "$(id -u)" = 0 ] || { printf '%s\n' 'deployment requires root' >&2; exit 1; }
stage=$(mktemp -d /run/soar-stage.XXXXXXXX)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
tar -xf - -C "$stage"
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
