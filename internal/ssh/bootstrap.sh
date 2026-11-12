# Fixed transport launcher. Input is a client-created archive with exactly the
# executor, its checksum, and the structured request. Never interpolate user data.
set -eu
[ "$(id -u)" = 0 ] || { printf '%s\n' 'bootstrap requires root' >&2; exit 1; }
stage=$(mktemp -d /run/soar-stage.XXXXXXXX)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
if ! tar -xf - -C "$stage"; then exit 1; fi
(cd "$stage" && sha256sum --status -c checksum)
chmod 700 "$stage/executor"
# From this point the worker owns cleanup. SSH loss must not delete its inputs or
# kill an in-flight package transaction. A fixed unit name rejects concurrency.
trap - EXIT HUP INT TERM
set +e
systemd-run --quiet --collect --wait --pipe --unit=soar-host-operation \
  --property=RuntimeMaxSec=21min --property=KillMode=control-group \
  /bin/sh -c 'stage=$1; trap '\''rm -rf "$stage"'\'' EXIT; "$stage/executor" internal-executor < "$stage/request.json"' sh "$stage"
status=$?
rm -rf "$stage"
exit "$status"
