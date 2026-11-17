# WAF, cache, and compression acceptance

Run `mise run test:integration --scenario waf-cache` on macOS with OrbStack.
The runner creates and deletes an isolated Ubuntu server; no existing server is
upgraded or mutated. It deploys the pinned production Caddy image and four real
Vamos services with the default HTTP middleware and each individual opt-out.

The origin returns a new UUID on every request, in both its body and a header.
The suite checks:

- Representative SQL injection and XSS probes return 403 with the WAF enabled,
  but reach the same origin with `waf: false`.
- Explicitly cacheable responses yield identical origin IDs and bodies plus a
  cache HIT header. `cache: false` yields distinct origin IDs instead.
- Origin `Cache-Control: no-store` is honored.
- Gzip is enabled by default and absent with `compression: false`.
- Nonempty files exist under the persistent Caddy cache directory.
- A cached response survives the runner's machine restart, with its original
  origin ID and a cache HIT header. The middleware probes run again afterward.

These probes demonstrate representative behavior, not comprehensive WAF attack
coverage. OrbStack machine restart need not restart its shared kernel. This
scenario does not deploy the new image to any previously registered live server.

Commands, headers, bodies, Caddy logs, and container inspection are collected in
`.pi/integration/<owned-machine>/guest-artifacts.tar`. A successful run includes
`waf-cache-summary.json`; the checkpoint records the pre-restart origin ID.
