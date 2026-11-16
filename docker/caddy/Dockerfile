FROM --platform=$BUILDPLATFORM caddy:2.11.9-builder@sha256:345264d8c6b50f44f4a4d507d4e6b6bcae40eef2f210d70f76e717fd8cc31baf AS builder
ARG TARGETOS
ARG TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH xcaddy build \
    --with github.com/corazawaf/coraza-caddy/v2@v2.6.2 \
    --with github.com/caddyserver/cache-handler@v0.17.0 \
    --with github.com/darkweak/storages/simplefs/caddy@v0.0.23

FROM caddy:2.11.9@sha256:4e2425f6009577e7c651010f16679c28f7d12df3e7b1914f941a7a3370746b3c
LABEL org.opencontainers.image.source="https://github.com/nchapman/vamos"
LABEL org.opencontainers.image.description="Vamos managed Caddy ingress with Coraza WAF and Souin HTTP cache"
LABEL org.opencontainers.image.licenses="Apache-2.0"
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
