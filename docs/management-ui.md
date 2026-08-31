# RedDotRelay management UI

The management UI is a Preact and TypeScript single-page application served by the RedDotRelay process at `/ui/`. Its compiled assets are copied into the same container image as the Go binary; there is no frontend server or second runtime process.

## Start and sign in

With the Compose service running, open `http://127.0.0.1:8080/ui/`. When the
database has no local users, the first-run form creates the initial
administrator. Subsequent visits use that local username and password. The UI
does not accept API keys for login; API keys are reserved for programmatic
Bearer clients.

An administrator sees configuration controls and both activity feeds. A read-only identity sees the dashboard and runtime events without mutation or audit controls.

## Dashboard and configuration

The dashboard combines public health/readiness with authenticated desired configuration and live runtime reconciliation. It reports delivery health, recent decoded events, operational activity, resource totals, and each RPC listener's state. The header reads the configured `environment.name` and release build information from the API.

Common administrator controls use the current strong revision automatically:

- Create and edit every scanner, retry, and TLS setting; RPC endpoints may be direct credential-free URLs or `env://NAME` and `file:///absolute/path` references.
- Test an RPC replacement and verify its reported chain ID before saving.
- Upload or paste contract ABI JSON, inspect canonical non-anonymous event signatures, and select events without entering UUIDs.
- Create, edit, and delete contracts plus global, RPC-listener, contract, and event webhook destinations.
- Configure HMAC-SHA256 with a referenced key and send a synthetic signed webhook test before saving.
- Durably pause or resume and delete an RPC listener.
- Search durable events by transaction, block, contract, signature, and delivery state; inspect each destination's attempt/retry status.
- Review scanner latest/confirmed/checkpoint/lag progress and requeue one confirmed dead destination.
- Review the durable, actor-attributed requeue audit separately from configuration audit.
- Load the complete `reddotrelay.config/v1` secret-safe JSON document and atomically replace it after editing nested webhooks, contracts, events, and HMAC settings.
- Manage local users and programmatic API keys. New identities are read-only by
  default; role promotion, disable/revoke, password reset, and key creation all
  require administrator confirmation.
- Inspect SQLite database/WAL/shared-memory size and disk usage, preview and run
  completed-event cleanup, optimize SQLite, and enable or disable automatic
  retention through APIs rather than direct database access.

RPC-listener, contract, and webhook deletion plus atomic replacement require confirmation. If another client advances the revision, the UI preserves the open form, refuses to overwrite the newer configuration, and asks for a refresh and review. Stored direct URLs are redacted to their origin; choose a replacement URL or reference before testing or updating one.

## Activity

Runtime events are drawn from a 1,000-entry in-memory ring and disappear when the process restarts. Only known static messages and approved fields are captured; arbitrary log text, raw errors, URLs, and secrets are excluded. Filter by severity or by server, scanner, and delivery components.

Configuration and delivery-requeue audit records are durable SQLite data written in the same transaction as each successful action. They are visible only to administrators. All activity views use cursor pagination.

## Session security

Cookie tokens and CSRF tokens contain 256 bits of randomness. Only the SHA-256
digest of each cookie token is persisted in SQLite. Cookies are HttpOnly and
`SameSite=Strict`; state-changing cookie requests additionally require an exact
same-origin header and constant-time CSRF-token validation.

Sessions expire after 30 minutes without use or eight hours after creation. The
server checks the backing local user on every request, so disabling the user
takes effect immediately and role changes are reflected without logging in
again. Unexpired sessions can survive process/container replacement when the
SQLite volume is preserved.

The Engine throttles each normalized username after five failed sign-in
attempts in a 15-minute window. Initial-administrator setup uses a separate
five-attempt window. Throttled requests return `429 Too Many Requests` with a
`Retry-After` header. Limiter entries are held only in bounded process memory,
contain hashed identifiers rather than usernames, expire automatically, and
reset after successful authentication. An Internet-facing reverse proxy must
still provide client-IP rate limiting because the Engine deliberately does not
trust forwarded-client-address headers.

For plain local HTTP, keep `server.ui_secure_cookies: false`. When HTTPS is terminated by a trusted reverse proxy, set it to `true`; otherwise the browser will reject the session cookie over HTTP. Preserve the original `Host` header through the proxy because RedDotRelay compares it with the browser `Origin` during login and mutations.

## Build locally

The development configuration serves `./ui/dist`. Build it before starting the Go service:

```console
npm --prefix ui ci
npm --prefix ui run check
npm --prefix ui run build
go run ./cmd/reddotrelay -config config.example.yaml
```

The Dockerfile performs the UI build in a Node stage, performs the Go build separately, then copies both outputs into a non-root distroless runtime image.
