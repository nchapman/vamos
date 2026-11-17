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
need_cmd openssl
need_cmd base64

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

# Cryptographically verify GitHub Actions SLSA build provenance attestation
# (Sigstore Fulcio CA chain, workflow SAN + OIDC issuer, and DSSE ECDSA signature).
attestation=$(curl -fsSL "https://api.github.com/repos/$REPO/attestations/sha256:$actual_hash" || true)
cert_b64=$(printf '%s' "$attestation" | sed -n 's/.*"rawBytes":"\([^"]*\)".*/\1/p')
payload_b64=$(printf '%s' "$attestation" | sed -n 's/.*"payload":"\([^"]*\)".*/\1/p')
sig_b64=$(printf '%s' "$attestation" | sed -n 's/.*"sig":"\([^"]*\)".*/\1/p')
att_time=$(printf '%s' "$attestation" | sed -n 's/.*"integratedTime":"\([0-9]*\)".*/\1/p')
if [ -z "$cert_b64" ] || [ -z "$payload_b64" ] || [ -z "$sig_b64" ] || [ -z "$att_time" ]; then
  echo "error: missing GitHub build provenance attestation for $archive" >&2
  exit 1
fi

decode_b64() {
  printf '%s' "$1" | (base64 -d 2>/dev/null || base64 -D 2>/dev/null)
}

cat > "$tmp_dir/fulcio-root.pem" <<'EOF'
-----BEGIN CERTIFICATE-----
MIIB+DCCAX6gAwIBAgITNVkDZoCiofPDsy7dfm6geLbuhzAKBggqhkjOPQQDAzAq
MRUwEwYDVQQKEwxzaWdzdG9yZS5kZXYxETAPBgNVBAMTCHNpZ3N0b3JlMB4XDTIx
MDMwNzAzMjAyOVoXDTMxMDIyMzAzMjAyOVowKjEVMBMGA1UEChMMc2lnc3RvcmUu
ZGV2MREwDwYDVQQDEwhzaWdzdG9yZTB2MBAGByqGSM49AgEGBSuBBAAiA2IABLSy
A7Ii5k+pNO8ZEWY0ylemWDowOkNa3kL+GZE5Z5GWehL9/A9bRNA3RbrsZ5i0Jcas
taRL7Sp5fp/jD5dxqc/UdTVnlvS16an+2Yfswe/QuLolRUCrcOE2+2iA5+tzd6Nm
MGQwDgYDVR0PAQH/BAQDAgEGMBIGA1UdEwEB/wQIMAYBAf8CAQEwHQYDVR0OBBYE
FMjFHQBBmiQpMlEk6w2uSu1KBtPsMB8GA1UdIwQYMBaAFMjFHQBBmiQpMlEk6w2u
Su1KBtPsMAoGCCqGSM49BAMDA2gAMGUCMH8liWJfMui6vXXBhjDgY4MwslmN/TJx
Ve/83WrFomwmNf056y1X48F9c4m3a3ozXAIxAKjRay5/aj/jsKKGIkmQatjI8uup
Hr/+CxFvaJWmpYqNkLDGRU+9orzh5hI2RrcuaQ==
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIB9zCCAXygAwIBAgIUALZNAPFdxHPwjeDloDwyYChAO/4wCgYIKoZIzj0EAwMw
KjEVMBMGA1UEChMMc2lnc3RvcmUuZGV2MREwDwYDVQQDEwhzaWdzdG9yZTAeFw0y
MTEwMDcxMzU2NTlaFw0zMTEwMDUxMzU2NThaMCoxFTATBgNVBAoTDHNpZ3N0b3Jl
LmRldjERMA8GA1UEAxMIc2lnc3RvcmUwdjAQBgcqhkjOPQIBBgUrgQQAIgNiAAT7
XeFT4rb3PQGwS4IajtLk3/OlnpgangaBclYpsYBr5i+4ynB07ceb3LP0OIOZdxex
X69c5iVuyJRQ+Hz05yi+UF3uBWAlHpiS5sh0+H2GHE7SXrk1EC5m1Tr19L9gg92j
YzBhMA4GA1UdDwEB/wQEAwIBBjAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBRY
wB5fkUWlZql6zJChkyLQKsXF+jAfBgNVHSMEGDAWgBRYwB5fkUWlZql6zJChkyLQ
KsXF+jAKBggqhkjOPQQDAwNpADBmAjEAj1nHeXZp+13NWBNa+EDsDP8G1WWg1tCM
WP/WHPqpaVo0jhsweNFZgSs0eE7wYI4qAjEA2WB9ot98sIkoF3vZYdd3/VtWB5b9
TNMea7Ix/stJ5TfcLLeABLE4BNJOsQ4vnBHJ
-----END CERTIFICATE-----
EOF

cat > "$tmp_dir/fulcio-intermediate.pem" <<'EOF'
-----BEGIN CERTIFICATE-----
MIICGjCCAaGgAwIBAgIUALnViVfnU0brJasmRkHrn/UnfaQwCgYIKoZIzj0EAwMw
KjEVMBMGA1UEChMMc2lnc3RvcmUuZGV2MREwDwYDVQQDEwhzaWdzdG9yZTAeFw0y
MjA0MTMyMDA2MTVaFw0zMTEwMDUxMzU2NThaMDcxFTATBgNVBAoTDHNpZ3N0b3Jl
LmRldjEeMBwGA1UEAxMVc2lnc3RvcmUtaW50ZXJtZWRpYXRlMHYwEAYHKoZIzj0C
AQYFK4EEACIDYgAE8RVS/ysH+NOvuDZyPIZtilgUF9NlarYpAd9HP1vBBH1U5CV7
7LSS7s0ZiH4nE7Hv7ptS6LvvR/STk798LVgMzLlJ4HeIfF3tHSaexLcYpSASr1kS
0N/RgBJz/9jWCiXno3sweTAOBgNVHQ8BAf8EBAMCAQYwEwYDVR0lBAwwCgYIKwYB
BQUHAwMwEgYDVR0TAQH/BAgwBgEB/wIBADAdBgNVHQ4EFgQU39Ppz1YkEZb5qNjp
KFWixi4YZD8wHwYDVR0jBBgwFoAUWMAeX5FFpWapesyQoZMi0CrFxfowCgYIKoZI
zj0EAwMDZwAwZAIwPCsQK4DYiZYDPIaDi5HFKnfxXx6ASSVmERfsynYBiX2X6SJR
nZU84/9DZdnFvvxmAjBOt6QpBlc4J/0DxvkTCqpclvziL6BCCPnjdlIB3Pu3BxsP
mygUY7Ii2zbdCdliiow=
-----END CERTIFICATE-----
EOF

decode_b64 "$cert_b64" | openssl x509 -inform DER -out "$tmp_dir/leaf.pem"
openssl verify -attime "$att_time" -CAfile "$tmp_dir/fulcio-root.pem" -untrusted "$tmp_dir/fulcio-intermediate.pem" "$tmp_dir/leaf.pem" >/dev/null
openssl x509 -in "$tmp_dir/leaf.pem" -noout -text > "$tmp_dir/leaf.txt"
if ! grep -Fq "URI:https://github.com/$REPO/.github/workflows/release.yml@refs/tags/v${version}" "$tmp_dir/leaf.txt" \
  || ! grep -A1 -F "1.3.6.1.4.1.57264.1.1:" "$tmp_dir/leaf.txt" | grep -Fq "https://token.actions.githubusercontent.com"; then
  echo "error: attestation certificate identity mismatch for $archive" >&2
  exit 1
fi

openssl x509 -in "$tmp_dir/leaf.pem" -pubkey -noout > "$tmp_dir/leaf-pub.pem"
decode_b64 "$payload_b64" > "$tmp_dir/payload.json"
decode_b64 "$sig_b64" > "$tmp_dir/sig.der"
payload_len=$(wc -c < "$tmp_dir/payload.json" | tr -d ' ')
printf 'DSSEv1 28 application/vnd.in-toto+json %s ' "$payload_len" > "$tmp_dir/pae.bin"
cat "$tmp_dir/payload.json" >> "$tmp_dir/pae.bin"
openssl dgst -sha256 -verify "$tmp_dir/leaf-pub.pem" -signature "$tmp_dir/sig.der" "$tmp_dir/pae.bin" >/dev/null

if ! grep -Fq "\"$actual_hash\"" "$tmp_dir/payload.json" \
  || ! grep -Fq '".github/workflows/release.yml"' "$tmp_dir/payload.json" \
  || ! grep -Fq "\"refs/tags/v${version}\"" "$tmp_dir/payload.json"; then
  echo "error: attestation provenance statement mismatch for $archive" >&2
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
