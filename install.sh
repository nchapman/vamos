#!/bin/sh
set -eu

REPO="${VAMOS_REPO:-nchapman/vamos}"
INSTALL_DIR="${VAMOS_INSTALL_DIR:-$HOME/.local/bin}"
REQUESTED_VERSION="${VAMOS_VERSION:-}"

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "error: required command '$1' not found" >&2
    exit 1
  fi
}

need_cmd curl
need_cmd tar
need_cmd uname
need_cmd mktemp

os_raw=$(uname -s)
case "$os_raw" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *)
    echo "error: unsupported OS '$os_raw' (download manually from https://github.com/$REPO/releases)" >&2
    exit 1
    ;;
esac

arch_raw=$(uname -m)
case "$arch_raw" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *)
    echo "error: unsupported architecture '$arch_raw'" >&2
    exit 1
    ;;
esac

if [ -n "$REQUESTED_VERSION" ]; then
  version="${REQUESTED_VERSION#v}"
else
  version=$(
    curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
      | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' \
      | head -n 1
  )
  if [ -z "$version" ]; then
    echo "error: could not determine latest release version from GitHub" >&2
    exit 1
  fi
fi

bundle="vamos_${version}_${os}_${arch}"
archive="${bundle}.tar.gz"
base_url="https://github.com/$REPO/releases/download/v${version}"

tmp_dir=$(mktemp -d)
stage_prefix=".vamos-install.$$"
cleanup() {
  rm -rf "$tmp_dir"
  for bin in vamos vamos-linux-amd64 vamos-linux-arm64; do
    rm -f "$INSTALL_DIR/${stage_prefix}.${bin}" 2>/dev/null || true
  done
}
trap cleanup EXIT INT TERM

echo "Downloading vamos v${version} (${os}/${arch})..."
curl -fsSL -o "$tmp_dir/SHA256SUMS" "$base_url/SHA256SUMS"
curl -fsSL -o "$tmp_dir/$archive" "$base_url/$archive"

expected_hash=$(awk -v f="$archive" '$2 == f { print $1; exit }' "$tmp_dir/SHA256SUMS")
if [ -z "$expected_hash" ]; then
  echo "error: checksum for $archive not found in SHA256SUMS" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  actual_hash=$(sha256sum "$tmp_dir/$archive" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  actual_hash=$(shasum -a 256 "$tmp_dir/$archive" | awk '{ print $1 }')
else
  echo "error: neither sha256sum nor shasum found to verify download" >&2
  exit 1
fi

if [ "$actual_hash" != "$expected_hash" ]; then
  echo "error: checksum mismatch for $archive" >&2
  exit 1
fi

# Validate archive member paths before extracting to prevent path traversal.
tar -tzf "$tmp_dir/$archive" | while IFS= read -r member; do
  case "$member" in
    /*|*../*|*/..|..)
      echo "error: unsafe path in archive: $member" >&2
      exit 1
      ;;
    "$bundle"|"$bundle/"|"$bundle/README.md"|"$bundle/vamos"|"$bundle/vamos-linux-amd64"|"$bundle/vamos-linux-arm64")
      ;;
    *)
      echo "error: unexpected file in archive: $member" >&2
      exit 1
      ;;
  esac
done

tar -C "$tmp_dir" -xzf "$tmp_dir/$archive" \
  "$bundle/vamos" "$bundle/vamos-linux-amd64" "$bundle/vamos-linux-arm64"

mkdir -p "$INSTALL_DIR"
for bin in vamos-linux-amd64 vamos-linux-arm64 vamos; do
  src="$tmp_dir/$bundle/$bin"
  if [ ! -f "$src" ] || [ -L "$src" ]; then
    echo "error: archive missing regular file $bin" >&2
    exit 1
  fi
  mode=0644
  if [ "$bin" = "vamos" ]; then
    mode=0755
  fi
  install -m "$mode" "$src" "$INSTALL_DIR/${stage_prefix}.${bin}"
done

for bin in vamos-linux-amd64 vamos-linux-arm64 vamos; do
  mv -f "$INSTALL_DIR/${stage_prefix}.${bin}" "$INSTALL_DIR/$bin"
done

echo "Installed vamos v${version} to $INSTALL_DIR"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    echo ""
    echo "Add $INSTALL_DIR to your PATH:"
    echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac
