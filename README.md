# RedDotRelay

RedDotRelay is a lightweight, self-hosted EVM event-to-webhook relay. It combines confirmed EVM scanning, ABI decoding, durable SQLite persistence, and reliable webhook delivery in one process.

RedDotRelay-owned code is available under [AGPL-3.0-only](LICENSE). See
[third-party notices](THIRD_PARTY_NOTICES.md), the [security policy](SECURITY.md),
[support policy](SUPPORT.md), and [contribution guide](CONTRIBUTING.md) before
redistributing or contributing.

The AGPL permits commercial use. Modified versions offered to users over a
network must provide those users the corresponding source as required by the
license. The separate RedDotRelay Cloud control plane is not part of this
repository or the Engine distribution.

Release artifacts remain drafts until their checksums, keyless signatures,
provenance, SBOMs, and container digest pass the
[release verification procedure](docs/release-verification.md).

Production adopters should also review the [known limits](docs/known-limits.md)
and [webhook consumer contract](docs/webhook-consumer.md).
See [Licensing and commercial use](docs/licensing.md) for the Engine/Cloud
boundary and [Rebuilding with modified go-ethereum](docs/rebuilding-with-modified-go-ethereum.md)
for the LGPL-compatible rebuild procedure.

RPC-listener configuration is managed through the REST API and stored in SQLite. YAML contains only static process settings and initial retention defaults, so API changes survive restarts and take effect without restarting RedDotRelay. Credential-bearing RPC and webhook URLs can instead be stored as `env://NAME` or `file:///absolute/path` references; resolved values never enter SQLite.

SQLite is the only supported database for the open-source RedDotRelay Engine. PostgreSQL is not an Engine dependency or planned alternate Engine backend; it is reserved for multi-tenant business and provisioning data in the optional commercial control plane.

## Requirements

- Go 1.25 or newer
- Node.js 24 or newer when building the management UI outside Docker
- Docker (optional)

## Run locally

Create the data directory, copy the static configuration, and start the service:

```powershell
New-Item -ItemType Directory -Force ./data | Out-Null
Copy-Item config.example.yaml config.yaml
npm --prefix ui ci
npm --prefix ui run build
go run ./cmd/reddotrelay -config config.yaml
```

Starting with an empty database is supported. `/healthz` becomes healthy when SQLite is available, `/readyz` becomes ready after the initial runtime reconciliation, and `/metrics` exposes Prometheus operational telemetry, even when no RPC listeners exist.

Open `http://127.0.0.1:8080/ui/`. On an empty database, the same-origin
first-run screen creates the initial local administrator. Sign in with that
username and password, then create the first RPC listener in the embedded
management console. Local users authenticate only the browser UI; API keys are
created separately for programmatic clients. See [Management UI](docs/management-ui.md)
for session security and [Management API](docs/management-api.md) for the
machine contract. Print the build version with
`go run ./cmd/reddotrelay -version`.

## Configuration ownership

Configuration has one owner for each concern:

- YAML owns the environment display name, HTTP listener, logging, SQLite path,
  delivery-worker settings, and optional initial retention policy.
- SQLite owns local users, browser sessions, API keys, RPC listeners, RPC and
  TLS settings, contracts, inline ABI JSON, selected events, webhook routes,
  retention overrides, UUIDs, revisions, timestamps, and audit records.
- The authenticated REST API is the supported way to create, patch, and delete
  RPC-listener configuration.

There is no YAML-to-SQLite synchronization or import. Legacy `chains`, `webhook_urls`, and listener-routing fields are rejected because YAML decoding is strict. Environment expansion applies to `storage.path`; management secrets and signed URLs are not stored in YAML.

Admin-managed RPC credentials are encrypted in SQLite. Set
`security.rpc_credentials_key_ref` to an `env://` or `file:///` reference before
creating authenticated RPC listeners; losing or changing that key makes the
stored credentials unreadable. Provider-JWT credentials use the provider HTTPS
token endpoint, `x-api-key`, and a precomputed RSA signature. OAuth2 is not part
of Engine v1.

The complete static schema is shown in `config.example.yaml`:

```yaml
environment:
  name: Local Engine

server:
  listen_address: ":8080"
  ui_directory: /ui
  ui_secure_cookies: false

log:
  level: info
  format: json

storage:
  path: /var/lib/reddotrelay/reddotrelay.db

delivery:
  workers: 4
  batch_size: 32
  http_timeout: 10s
  lease_duration: 30s
  retry_backoff: 1s
  max_backoff: 5m
  max_attempts: 20
  poll_interval: 1s
```

The management API shares `server.listen_address` with `/healthz` and `/readyz`. Bind it to loopback when only local tools need access. For remote management, use a private network and terminate HTTPS at a trusted reverse proxy. `/healthz`, `/readyz`, and `/metrics` are public; `/api/v1/**` operations require either a same-origin UI session or an API key, except initial UI setup and session creation.

## Local users and API keys

The management UI authenticates local username/password accounts and uses an
HttpOnly, `SameSite=Strict` session cookie plus a CSRF token. Local users have
`admin` or `read-only` roles; new users default to `read-only`, and all
mutations are disabled for them in the UI and rejected by the API.

Programmatic API clients authenticate with `Authorization: Bearer <secret>`.
API keys also have `admin` or `read-only` roles and new keys default to
`read-only` until explicitly promoted.

Manage per-client keys locally on a host that can access the configured SQLite file:

```powershell
go run ./cmd/reddotrelay api-key create -config config.yaml -name operations -role admin
go run ./cmd/reddotrelay api-key create -config config.yaml -name dashboard -role read-only
go run ./cmd/reddotrelay api-key list -config config.yaml
go run ./cmd/reddotrelay api-key revoke -config config.yaml -id <key-uuid>
```

API-key secrets are random 256-bit values and are returned only at creation. SQLite stores only their hashes and safe prefixes. Use one key per client so individual access can be revoked and mutations can be attributed in the audit log.

The UI includes confirmed user and API-key management. The equivalent offline
user commands are `reddotrelay user create|list|enable|disable|password-reset`.

See [Management API](docs/management-api.md) for every route, request shape, optimistic-concurrency behavior, webhook inheritance, redaction rules, runtime reconciliation, and audit pagination.

## Database backup and recovery

Create a transactionally consistent, verified SQLite backup without stopping the service:

```powershell
go run ./cmd/reddotrelay database backup -config config.yaml -output ./reddotrelay-backup.db
```

Restore is an offline operation and requires explicit confirmation that the service is stopped:

```powershell
go run ./cmd/reddotrelay database restore -config config.yaml -input ./reddotrelay-backup.db -confirm-service-stopped
```

See [SQLite backup, restore, upgrade, and rollback](docs/database-backup-restore.md) for Docker commands, verification steps, and rollback rules.

## Project layout

```text
cmd/reddotrelay/   service, operational CLI, management API, UI/session runtime
docs/              management and reliability procedures
internal/auth/     API-key secret generation and hashing
internal/config/   strict static YAML loading and validation
internal/core/     domain models and component interfaces
internal/decoder/  inline ABI decoding and batch persistence
internal/delivery/ durable outbox webhook workers
internal/logging/  structured logging setup
internal/scanner/  confirmed, batched EVM log scanning and reorg recovery
internal/store/    SQLite configuration, events, checkpoints, and outbox
ui/                Preact/TypeScript management console built into the image
```

The SQLite store atomically persists events, initial delivery rows, and scanner checkpoints. Event identity is `(chain_id, transaction_hash, log_index)`, so repeated RPC results are idempotent. The webhook `eventId` and `Idempotency-Key` are the same deterministic UUID v5 derived from that identity.

Webhook workers durably claim pending deliveries, fence state changes with a claim token, use bounded concurrency and HTTP timeouts, and retry failures with capped exponential backoff. Exhausted deliveries become dead letters. Webhook failure cannot block checkpoint advancement, and a restart resumes pending work after its persisted lease time.

Resolved destinations are persisted with an event when it is first stored. Later routing changes affect future events and never rewrite existing outbox rows.

## Dead-letter operations

Inspect deliveries that exhausted their retry limit:

```powershell
go run ./cmd/reddotrelay dead-letter list -config config.yaml
```

After fixing the receiver, requeue every dead delivery for one event UUID:

```powershell
go run ./cmd/reddotrelay dead-letter requeue -config config.yaml `
  -event-id bd493388-c02e-5299-aa84-6d8504c48905
```

A bulk requeue requires explicit confirmation:

```powershell
go run ./cmd/reddotrelay dead-letter requeue -config config.yaml -all -confirm
```

Requeue resets delivery attempts and stale lease state but preserves the event. Offline operational commands read only `storage.path`; they do not need RPC or webhook secrets.

## Retention operations

Preview successfully delivered events older than 90 days:

```powershell
go run ./cmd/reddotrelay retention prune -config config.yaml -older-than 2160h
```

After reviewing the count and taking an SQLite-aware backup, delete them:

```powershell
go run ./cmd/reddotrelay retention prune -config config.yaml -older-than 2160h -confirm
```

Only events whose deliveries all succeeded before the cutoff are removed. Checkpoints and pending, leased, retrying, or dead-letter deliveries are preserved. Pruning uses short bounded transactions and can run while the service is active. A deep reorg into pruned history can redeliver an event with its original deterministic ID, so consumers must continue to deduplicate by `eventId`.

Automatic retention is disabled by default. Administrators can enable or
disable it in **Storage & retention**; enabling without custom values uses 720
hours, a one-hour interval, and 1,000-event batches. The policy is persisted in
SQLite and is also manageable with `reddotrelay retention config`. An optional
YAML `retention` block supplies the initial policy before a SQLite override is
saved.

The emergency CLI and management API use the same retention service and eligibility rules. Inspect the database footprint with `reddotrelay database status`; run `reddotrelay database optimize -config config.yaml -confirm` to checkpoint the WAL and apply SQLite's safe optimizer without deleting events. The authenticated management API exposes `GET /api/v1/storage/status`, `GET /api/v1/retention/status`, `POST /api/v1/retention/preview`, `POST /api/v1/retention/prune`, and `POST /api/v1/storage/optimize`. Destructive API requests require an admin principal and `{"confirm":true}`.

`/api/v1/rpc-listeners` is the canonical v1 management route. Pre-v1
`/api/v1/rpc-listeners` aliases are implementation compatibility aids and are
not part of the v1 public contract.

## Reliability

The scanner polls `eth_getLogs` over bounded confirmed ranges, resumes from durable checkpoints, retries transient RPC failures, verifies block hashes and ancestry, and safely deduplicates repeated logs. WebSockets are not part of the correctness path.

A detected reorg resets events, outbox rows, and the checkpoint inclusively from the configured start block, then rescans. Webhooks already accepted by a consumer cannot be undone; RedDotRelay does not emit compensating reorg notifications.

The repository includes unit and integration coverage for checkpoint recovery,
reorg handling, idempotent persistence, durable delivery, authentication, and
management operations.

## Validate

```sh
go fmt ./...
go test ./...
go vet ./...
go build ./...
```

On Windows:

```powershell
.\scripts\validate.ps1
```

The script uses an ignored workspace-local Go build cache.

## Container

```sh
docker build -t reddotrelay .
docker run --rm -p 8080:8080 -v reddotrelay-data:/var/lib/reddotrelay reddotrelay
```

The volume retains local users, hashed sessions, API-key hashes, RPC-listener
configuration, retention settings, checkpoints, pending deliveries, and audit
history across container replacement. Start Compose, then create the initial
administrator in the browser at `http://127.0.0.1:8080/ui/`:

```powershell
docker compose up --build -d
```

Keep API-key and signed webhook secrets outside Compose files and source control.
