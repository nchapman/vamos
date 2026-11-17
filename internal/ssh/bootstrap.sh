# Fixed transport launcher. Input is a client-created archive with exactly the
# executor, its checksum, and the structured request. Never interpolate user data.
set -eu
[ "$(id -u)" = 0 ] || { printf '%s\n' 'bootstrap requires root' >&2; exit 1; }
stage=$(mktemp -d /run/vamos-stage.XXXXXXXX)
execdir=$(mktemp -d /var/lib/.vamos-exec.XXXXXXXX)
trap 'rm -rf "$stage" "$execdir"' EXIT HUP INT TERM
if ! tar -xf - -C "$stage"; then exit 1; fi
(cd "$stage" && sha256sum --status -c checksum)
mv "$stage/executor" "$execdir/executor"
chmod 700 "$execdir/executor"
# From this point the worker owns cleanup. SSH loss must not delete its inputs or
# kill an in-flight package transaction. A fixed unit name rejects concurrency.
trap - EXIT HUP INT TERM
set +e
systemd-run --quiet --collect --wait --pipe --unit=vamos-host-operation \
  --setenv="SSH_CONNECTION=${SSH_CONNECTION:-}" \
  --property=RuntimeMaxSec=21min --property=KillMode=control-group \
  /bin/sh -c 'stage=$1; execdir=$2; trap '\''rm -rf "$stage" "$execdir"'\'' EXIT; "$execdir/executor" internal-executor < "$stage/request.json"' sh "$stage" "$execdir"
status=$?
rm -rf "$stage" "$execdir"
exit "$status"
