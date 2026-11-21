#!/bin/sh
# Shared by the local formatter, CI, and the Linux guest (which has no .git).
set -eu
case "${1:-}" in
  -l|-w) ;;
  *) echo 'usage: sh integration/gofmt.sh -l|-w' >&2; exit 2 ;;
esac

find cmd internal integration -type d \( -name node_modules -o -name '.*' \) -prune -o \
  -type f -name '*.go' ! -name '.*' -exec gofmt "$1" {} +
