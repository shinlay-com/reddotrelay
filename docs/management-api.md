# RedDotRelay management API

The management API owns live RPC-listener configuration. It is served from the same listener as health endpoints and persists every successful mutation to SQLite before notifying the scanner runtime manager. `/api/v1/rpc-listeners` is the only supported v1 route family; legacy signal-config routes are removed.

The machine-readable API contract is maintained in [`api/openapi.yaml`](../api/openapi.yaml). Contract tests parse it, resolve every internal reference, enforce revision preconditions on mutations, and detect method/path drift.

## Authentication and roles

Programmatic API clients authenticate to `/api/v1/**` with a per-client bearer key:

```http
Authorization: Bearer api_key_<secret>
```

Create keys locally with access to the same SQLite database:

```console
go run ./cmd/reddotrelay api-key create -config config.yaml -name operations -role admin
go run ./cmd/reddotrelay api-key create -config config.yaml -name dashboard -role read-only
go run ./cmd/reddotrelay api-key list -config config.yaml
go run ./cmd/reddotrelay api-key revoke -config config.yaml -id <key-uuid>
```

The `admin` role can perform every operation. The `read-only` role can read
RPC-listener configuration, runtime/scanner progress, operational events,
storage status, and durable event/delivery diagnostics. Configuration and
requeue-audit access plus all mutations require `admin`. New API keys default
to `read-only`; promotion is a separate confirmed administrator action.

The embedded UI uses local username/password accounts, not API keys. On an
empty database, `GET /api/v1/ui-setup` reports that setup is required and the
same-origin `POST /api/v1/ui-setup` creates the initial administrator exactly
once. `POST /api/v1/ui-session` authenticates a local user. The response sets
an HttpOnly, `SameSite=Strict` cookie and returns a CSRF token;
cookie-authenticated mutations require that token in `X-CSRF-Token` plus the
same-origin header. Sessions have a 30-minute idle lifetime and an eight-hour
absolute lifetime. Only their token hashes are stored in SQLite, so unexpired
sessions can survive process replacement; disabling the backing user
invalidates access immediately. Set `server.ui_secure_cookies: true` behind
HTTPS. Bearer clients continue to work without cookies or CSRF headers.

Administrators manage local users through `/api/v1/users` and programmatic keys
through `/api/v1/api-keys`. New users and keys are created as `read-only`.
Enable/disable, password reset, role change, key role change, and key revocation
are explicit confirmed operations.

The creation command returns a random secret only once. SQLite retains its hash, safe prefix, identity, role, creation time, last-use time, and optional revocation time. Give each client a separate key and deliver it through a secret manager. Do not put keys in URLs, YAML, logs, shell history, or source control.

The API does not terminate TLS itself. For access beyond loopback, use a private network and a trusted HTTPS reverse proxy.

## RPC authentication

RPC listeners support no authentication, HTTP Basic, a static Bearer token, a
custom header, Engine JWT, and provider JWT. OAuth2 is intentionally out of
scope for Engine v1. Authentication secrets are accepted only by administrator
mutations, encrypted in SQLite with the key referenced by
`security.rpc_credentials_key_ref`, and never returned in API responses,
exports, audit records, or logs.

For provider JWT, the administrator supplies the provider HTTPS token endpoint,
the `x-api-key` value, and a precomputed RSA-SHA256 signature. RedDotRelay
sends `{"signature":"..."}` with `Content-Type: application/json`, reads the
returned JWT `exp` claim, and caches the token until a proactive refresh margin
is reached. Concurrent requests share one refresh. A single RPC `401` causes
one token refresh and request replay when the request body is replayable.

Provider credentials cannot be exported because the API key and signature are
write-only. Remove RPC authentication before exporting a configuration, then
restore it through an administrator mutation after import.

## Revisions and request rules

All resources share one monotonically increasing configuration revision. A successful `GET /api/v1/rpc-listeners` or `GET /api/v1/rpc-listeners/{listenerId}` returns:

```http
ETag: "revision-12"
```

Every `POST`, `PATCH`, and `DELETE` must include that exact value:

```http
If-Match: "revision-12"
```

This prevents one client from silently overwriting another client's work. A missing precondition returns `428 Precondition Required`. A stale precondition returns `412 Precondition Failed` with the current `ETag`; fetch the collection again, reconsider the change against the new state, and retry.

Create bodies use `Content-Type: application/json`. Patches use `Content-Type: application/merge-patch+json`. Bodies are limited to 4 MiB, unknown fields are rejected, and each request must contain exactly one JSON object.

Server-generated resource IDs are canonical UUIDs. `POST` returns `201 Created`, the new resource and UUID, `Location`, and the new `ETag`. `PATCH` returns `200 OK` and the updated resource. `DELETE` returns `204 No Content`, the new `ETag`, and `X-Config-Revision`.

### Postman collection setup

Create collection variables named `baseUrl`, `apiKey`, `etag`, and `listenerId`. Set `baseUrl` to `http://127.0.0.1:8080`, keep the API-key secret in the local current value of `apiKey`, and initially leave the other two empty. Configure collection authorization as **Bearer Token** with `{{apiKey}}`.

Send `GET {{baseUrl}}/api/v1/rpc-listeners`, then use this **Scripts** > **Post-response** script to capture the current revision including its required quotes:

```javascript
const etag = pm.response.headers.get("ETag");
pm.expect(etag, "ETag response header").to.exist;
pm.collectionVariables.set("etag", etag);
```

Add `If-Match: {{etag}}` to every mutation. Add the same Post-response script to mutation requests so each successful response replaces `etag` with the new revision. If a request returns `412`, fetch the collection again to refresh the revision before retrying.

## Resource paths

Only RPC listeners have individual `GET` routes. Nested resources are returned in their parent listener representation.

| Resource | Create | Patch | Delete |
| --- | --- | --- | --- |
| RPC listener | `POST /api/v1/rpc-listeners` | `PATCH /api/v1/rpc-listeners/{listenerId}` | `DELETE /api/v1/rpc-listeners/{listenerId}` |
| Global webhook | `POST /api/v1/rpc-listeners/webhooks` | `PATCH /api/v1/rpc-listeners/webhooks/{webhookId}` | `DELETE /api/v1/rpc-listeners/webhooks/{webhookId}` |
| RPC-listener webhook | `POST /api/v1/rpc-listeners/{listenerId}/webhooks` | `PATCH /api/v1/rpc-listeners/{listenerId}/webhooks/{webhookId}` | `DELETE /api/v1/rpc-listeners/{listenerId}/webhooks/{webhookId}` |
| Contract | `POST /api/v1/rpc-listeners/{listenerId}/contracts` | `PATCH /api/v1/rpc-listeners/{listenerId}/contracts/{contractId}` | `DELETE /api/v1/rpc-listeners/{listenerId}/contracts/{contractId}` |
| Contract webhook | `POST /api/v1/rpc-listeners/{listenerId}/contracts/{contractId}/webhooks` | `PATCH /api/v1/rpc-listeners/{listenerId}/contracts/{contractId}/webhooks/{webhookId}` | `DELETE /api/v1/rpc-listeners/{listenerId}/contracts/{contractId}/webhooks/{webhookId}` |
| Event | `POST /api/v1/rpc-listeners/{listenerId}/contracts/{contractId}/events` | `PATCH /api/v1/rpc-listeners/{listenerId}/contracts/{contractId}/events/{eventId}` | `DELETE /api/v1/rpc-listeners/{listenerId}/contracts/{contractId}/events/{eventId}` |
| Event webhook | `POST /api/v1/rpc-listeners/{listenerId}/contracts/{contractId}/events/{eventId}/webhooks` | `PATCH /api/v1/rpc-listeners/{listenerId}/contracts/{contractId}/events/{eventId}/webhooks/{webhookId}` | `DELETE /api/v1/rpc-listeners/{listenerId}/contracts/{contractId}/events/{eventId}/webhooks/{webhookId}` |

Read and operational routes are:

| Method and path | Role | Purpose |
| --- | --- | --- |
| `GET /api/v1/rpc-listeners` | read-only or admin | Complete snapshot, global webhooks, revision, and update time |
| `GET /api/v1/rpc-listeners/{listenerId}` | read-only or admin | One nested RPC-listener aggregate at the global revision |
| `GET /api/v1/rpc-listener-status` | read-only or admin | Desired revision and live scanner reconciliation state |
| `GET /api/v1/rpc-listener-audit` | admin | Newest-first mutation audit page |
| `GET /api/v1/operational-events` | read-only or admin | Bounded newest-first secret-safe runtime event page |
| `GET /api/v1/rpc-listener-export` | admin | Deterministic, versioned desired-state export |
| `PUT /api/v1/rpc-listener-import` | admin | Atomic full desired-state replacement |
| `POST /api/v1/rpc-listeners/{listenerId}/pause` | admin | Durably stop new scanning for one RPC listener |
| `POST /api/v1/rpc-listeners/{listenerId}/resume` | admin | Resume scanning from its durable checkpoint |
| `GET /api/v1/events` | read-only or admin | Filtered, cursor-paginated decoded event history |
| `GET /api/v1/events/{eventId}/deliveries` | read-only or admin | Persisted destinations and attempt state for one event |
| `POST /api/v1/deliveries/{deliveryId}/requeue` | admin | Confirmed targeted dead-letter requeue |
| `GET /api/v1/delivery-requeue-audit` | admin | Durable actor-attributed requeue audit |
| `GET /api/v1/storage/status` | read-only or admin | SQLite files, volume space, and delivery counts |
| `POST /api/v1/storage/optimize` | admin | Confirmed WAL checkpoint and SQLite optimization |
| `GET /api/v1/retention/status` | read-only or admin | Persisted automatic-retention policy and runtime state |
| `POST /api/v1/retention/config` | admin | Confirmed automatic-retention enable/disable |
| `POST /api/v1/retention/preview` | read-only or admin | Count eligible completed events without deletion |
| `POST /api/v1/retention/prune` | admin | Confirmed bounded completed-event deletion |
| `GET /api/v1/build-info` | read-only or admin | Version, commit, build date, and environment name |
| `GET /healthz` | public | Minimal SQLite liveness result |
| `GET /readyz` | public | Minimal initial-reconciliation/readiness result |
| `GET /metrics` | public | Prometheus text exposition for production operations |

## Prometheus metrics

`GET /metrics` is unauthenticated so a Prometheus server can scrape it without a management API key. It shares `server.listen_address`; restrict the listener or the `/metrics` path with network policy or a trusted reverse proxy when operational telemetry must not be public.

RedDotRelay publishes:

- `reddotrelay_build_info`
- `reddotrelay_config_revision`
- `reddotrelay_runtime_listeners` and `reddotrelay_runtime_build_failures_total`
- `reddotrelay_latest_block`, `reddotrelay_confirmed_head_block`, `reddotrelay_checkpoint_block`, and `reddotrelay_scanner_lag_blocks`
- `reddotrelay_scanner_cycles_total`, `reddotrelay_rpc_requests_total`, `reddotrelay_events_processed_total`, and `reddotrelay_reorgs_total`
- `reddotrelay_delivery_attempts_total` and durable `reddotrelay_deliveries` counts
- Standard Go runtime/process build collectors from the Prometheus Go client

Labels are intentionally bounded to RPC-listener UUID, chain ID, enumerated operation, outcome, state, delivery status, and build version. URLs, destinations, secret references, transaction hashes, event IDs, contract addresses, API-key identities, and error text are never labels.

## Atomic configuration export and import

`GET /api/v1/rpc-listener-export` returns a deterministic `reddotrelay.config/v1` JSON document containing the complete desired configuration, stable resource UUIDs, secret references, and ordering. It omits the live revision, timestamps, audit history, runtime status, checkpoints, events, and deliveries. The response ETag identifies the revision from which it was produced.

An export refuses any direct URL containing user information, a non-root path, query, or fragment because RedDotRelay cannot reliably distinguish a credential from ordinary URL data. Replace such values with `rpcUrlRef` or `urlRef`; the endpoint never silently leaks or redacts a value in a purportedly round-trippable backup.

In Postman, send `GET {{baseUrl}}/api/v1/rpc-listener-export`, save the response body to a file or variable, and capture its ETag with the standard Post-response script. To restore it, send `PUT {{baseUrl}}/api/v1/rpc-listener-import` with `Content-Type: application/json`, `If-Match: {{etag}}`, and the unchanged exported JSON as the raw body.

Import is full desired-state replacement, not merge. RedDotRelay rejects unknown fields, unsupported schema versions, malformed or duplicate UUIDs, unsafe direct URLs, invalid ABIs/routes/references, and stale revisions before writing. SQLite then deletes the old configuration aggregate, inserts the complete replacement, advances the revision once, and records one `import`/`configuration` audit entry in a single transaction. Any failure rolls back the entire replacement. Operational checkpoints, events, deliveries, and delivery history are not part of the import and remain intact.

## Durable pause and resume

In Postman, pause with `POST {{baseUrl}}/api/v1/rpc-listeners/{{listenerId}}/pause` and resume with `POST {{baseUrl}}/api/v1/rpc-listeners/{{listenerId}}/resume`. Both requests have no body and require `If-Match: {{etag}}`. Capture each response ETag before the next mutation.

Pause is durable desired state: it stops that RPC listener's scanner without deleting its checkpoint, events, or queued deliveries. The independent webhook worker continues retrying deliveries that were already persisted. Resume constructs the scanner again and continues from the durable checkpoint. Each state change advances the global revision and records a `pause` or `resume` audit entry; repeating the requested state is idempotent and keeps the current revision. Export and import preserve the `paused` boolean.

## Create a complete RPC listener

First send `GET {{baseUrl}}/api/v1/rpc-listeners` to populate `etag`. Then create a chain, inline contract ABI, selected event, and chain-level webhook atomically with `POST {{baseUrl}}/api/v1/rpc-listeners`. Set `Content-Type: application/json` and `If-Match: {{etag}}`, then use this raw JSON body:

```json
{
  "name": "private-besu",
  "chainId": 9171317,
  "rpcUrl": "http://127.0.0.1:8545",
  "startBlock": 0,
  "batchSize": 2000,
  "pollInterval": "2s",
  "confirmations": 2,
  "reorgDepth": 12,
  "rpcRetryAttempts": 5,
  "rpcRetryBackoff": "500ms",
  "rpcTimeout": "15s",
  "webhooks": [
    { "url": "http://127.0.0.1:8081/reddotrelay" }
  ],
  "contracts": [
    {
      "address": "0x0000000000000000000000000000000000000001",
      "abi": [
        {
          "anonymous": false,
          "inputs": [
            { "indexed": true, "name": "from", "type": "address" },
            { "indexed": true, "name": "to", "type": "address" },
            { "indexed": false, "name": "value", "type": "uint256" }
          ],
          "name": "Transfer",
          "type": "event"
        }
      ],
      "events": [
        { "selector": "Transfer(address,address,uint256)" }
      ]
    }
  ]
}
```

Use this Post-response script to save the created RPC-listener ID and new revision:

```javascript
pm.response.to.have.status(201);
pm.collectionVariables.set("etag", pm.response.headers.get("ETag"));
pm.collectionVariables.set("listenerId", pm.response.json().rpcListener.id);
```

All scanner durations use Go duration strings. `chainId` must be unique and non-zero; `batchSize`, `reorgDepth`, retry counts, and durations must be positive. RPC and webhook destinations must be absolute HTTP(S) URLs. Contract addresses must be lowercase or EIP-55 checksummed. An event `selector` must match exactly one non-anonymous canonical event signature in its contract ABI.

## Secret references

RPC and webhook URLs can be loaded at runtime instead of stored in SQLite. Use exactly one of `rpcUrl` or `rpcUrlRef`, and exactly one of `url` or `urlRef` for each webhook:

```json
{
  "rpcUrlRef": "env://CHAIN_RPC_URL",
  "webhooks": [
    { "urlRef": "file:///run/secrets/reddotrelay_webhook_url" }
  ]
}
```

Supported references are `env://VARIABLE_NAME` and `file:///absolute/path`. File values are limited to 64 KiB and trailing CR/LF bytes are ignored to support mounted container secrets. Reference identifiers are durable configuration and are returned by the API; resolved values are never stored, returned, logged, or included in audit records. Referenced webhook destinations remain references in durable delivery rows and are resolved for every attempt, so file-based credentials can rotate while deliveries are queued. Referenced RPC URLs are resolved when a scanner runtime is constructed; restart the service or update the RPC-listener configuration after rotating an RPC URL.

Environment variables are inherited when RedDotRelay starts and cannot normally be changed from outside the running process. Prefer mounted files when live webhook credential rotation is required. Protect both environment access and referenced files with operating-system permissions.

## Webhook HMAC authentication

Any webhook can opt into HMAC-SHA256 authentication. The signing key must be a secret reference; plaintext signing keys are rejected:

```json
{
  "url": "https://receiver.example/events",
  "authentication": {
    "type": "hmac-sha256",
    "secretRef": "file:///run/secrets/receiver_hmac_key",
    "keyId": "receiver-2026-08"
  }
}
```

For each attempt, RedDotRelay sends:

- `RedDotRelay-Timestamp`: current Unix time in seconds.
- `RedDotRelay-Signature`: `v1=` followed by lowercase hexadecimal HMAC-SHA256.
- `RedDotRelay-Key-Id`: the optional public `keyId`.

The signed bytes are the ASCII timestamp, one period (`.`), and the exact JSON request body: `timestamp + "." + body`. Receivers should reject stale timestamps, decode the hexadecimal digest, calculate HMAC-SHA256 over the exact unmodified body, and compare digests with a constant-time function. `Idempotency-Key` remains the stable RedDotRelay event ID.

The authentication type, secret reference, and key ID are copied into the durable delivery row atomically with the event. They therefore remain stable if the webhook configuration is later edited or deleted. The resolved key is never persisted. Each retry resolves the referenced key again and uses a fresh timestamp, allowing mounted-file key rotation to affect queued deliveries.

## Create nested resources

Nested create bodies contain only the resource being added. For a contract:

```json
{
  "address": "0x0000000000000000000000000000000000000001",
  "abi": [],
  "webhooks": [{ "url": "https://receiver.example/contract" }],
  "events": []
}
```

For an event:

```json
{
  "selector": "Transfer(address,address,uint256)",
  "webhooks": [{ "url": "https://receiver.example/transfer" }]
}
```

For any webhook collection:

```json
{ "url": "https://receiver.example/hook?signature=secret" }
```

An empty ABI is valid JSON but an event cannot be added until the ABI contains its definition. A configuration may contain an RPC listener with no contracts, or a contract with no events. Every configured event must resolve to at least one webhook.

## Targeted merge patches

A patch changes only the supplied fields; it never requires the complete aggregate. Child collections are changed through their own create/delete endpoints rather than embedded in a parent patch.

Mutable RPC-listener fields are `name`, `chainId`, `rpcUrl`, `rpcUrlRef`, `startBlock`, `batchSize`, `pollInterval`, `confirmations`, `reorgDepth`, `rpcRetryAttempts`, `rpcRetryBackoff`, `rpcTimeout`, and `tls`. Contract fields are `address`, `abi`, and `eventSelectors`. The event field is `selector`, and webhook fields are `url`, `urlRef`, and `authentication`. Setting a URL replaces and clears its reference; setting a reference replaces and clears its direct URL. A single patch cannot set both alternatives. Setting `authentication` replaces the complete authentication object; setting it to `null` disables signing.

`eventSelectors` atomically reconciles a contract's selected events in the supplied order. Events whose selector remains selected preserve their UUIDs and nested webhooks; new selectors receive server-generated UUIDs, and omitted selectors are removed. Send it with `abi` in the same patch when changing an ABI so the candidate is validated as one revision. Null, blank, duplicate, ambiguous, anonymous, or ABI-missing selectors are rejected without changing configuration.

Update a contract ABI and its event selections atomically by sending `PATCH {{baseUrl}}/api/v1/rpc-listeners/{{listenerId}}/contracts/{{contractId}}` with `Content-Type: application/merge-patch+json`, `If-Match: {{etag}}`, and this raw body:

```json
{"abi":[{"anonymous":false,"inputs":[],"name":"Ping","type":"event"}],"eventSelectors":["Ping()"]}
```

Update only an event selector:

```json
{ "selector": "Approval(address,address,uint256)" }
```

Replace only a webhook destination:

```json
{ "url": "https://receiver.example/v2?signature=new-secret" }
```

Omitted fields remain unchanged. Required fields cannot be `null`. Within `tls`, optional `caPem` and `serverName` can be cleared with `null`; `insecureSkipVerify` cannot. Empty patches, immutable fields such as `id` or timestamps, and unknown fields are rejected.

## Webhook inheritance

Webhook routing uses replacement inheritance. For each event, the first non-empty list in this order is selected:

1. Event webhooks
2. Contract webhooks
3. Chain webhooks
4. Global webhooks

Routes are not accumulated. Multiple destinations at the selected level create independent durable delivery rows. `GET` returns the explicit `webhooks`, the resolved `effectiveWebhooks`, and a `webhookSource` of `event`, `contract`, `chain`, `global`, or `none`.

Deleting an explicit override restores inheritance from the next level. A mutation that would leave any configured event without an effective route is rejected with `422 Unprocessable Entity` and changes neither data nor revision.

## Redaction and secret-preserving changes

RPC and webhook URLs may contain credentials in user information, paths, queries, or fragments. API responses therefore return only the URL origin. For example, stored `https://user:pass@example.test/hook-token?sig=secret` is returned as `https://example.test`.

The stored value is not changed by redaction. An unrelated patch preserves the original secret because omitted fields remain unchanged. Do not copy a redacted response URL into a patch: supplying `rpcUrl` or `url` means replace the entire stored value, so the client must send the complete new URL including any required secret.

API keys are never returned by the REST API. Audit records store identities and resource metadata, not request bodies, ABIs, RPC URLs, webhook URLs, or secrets.

## Delete behavior

In Postman, use the `DELETE` resource path from the table, add `If-Match: {{etag}}`, and send no body. For example, delete a RPC listener with `DELETE {{baseUrl}}/api/v1/rpc-listeners/{{listenerId}}`, or delete an event with `DELETE {{baseUrl}}/api/v1/rpc-listeners/{{listenerId}}/contracts/{{contractId}}/events/{{eventId}}`. A successful deletion returns `204 No Content`; capture its response ETag with the standard script.

Deleting a RPC listener, contract, or event stops future scanning for that resource after reconciliation and removes only its configuration rows. Existing checkpoints, persisted events, deliveries, and delivery history are preserved. There is no implicit operational-data purge; use the explicit retention command for eligible delivered history.

## Runtime reconciliation and readiness

A successful mutation means SQLite committed the desired configuration. Scanner reconciliation follows asynchronously, normally from an immediate in-process notification and with periodic polling as recovery.

For a valid replacement, RedDotRelay builds the candidate first, then stops the previous scanner and starts the replacement without overlapping two scanners for the same RPC-listener UUID. If construction fails, it keeps an existing healthy scanner running when possible and retries the desired configuration with bounded backoff. A new RPC listener whose runtime cannot be built has no scanner until a retry succeeds. Secrets are not exposed in status or runtime logs; operational failures use bounded safe summaries.

Inspect convergence in Postman with `GET {{baseUrl}}/api/v1/rpc-listener-status`.

The response includes `desiredRevision`, `initialReconcileComplete`, `lastReconciledAt`, `ready`, and one entry per RPC listener. States are:

- `running`: a scanner is active for the desired configuration.
- `idle`: the desired RPC listener has no selected events, so no RPC scanning is needed.
- `paused`: scanning is intentionally disabled in durable desired state.
- `build-failed`: the first runtime construction attempt failed.
- `retrying`: repeated construction attempts are waiting or in progress.

Failure entries include a safe `lastError`, attempt count, and `nextRetryAt`. `/readyz` returns `200 {"status":"ready"}` only after initial reconciliation and while every desired RPC listener is running, idle, or intentionally paused. It returns `503 {"status":"not_ready"}` for build failures. `/healthz` independently reports SQLite liveness as `200 {"status":"ok"}` or `503 {"status":"unhealthy"}`. Both endpoints are intentionally unauthenticated and minimal.

## Audit pagination

Each successful mutation writes its audit entry in the same SQLite transaction as the configuration change. In Postman, fetch the newest records with `GET {{baseUrl}}/api/v1/rpc-listener-audit?limit=50` and an admin key.

`limit` defaults to 50 and must be between 1 and 200. Entries contain actor ID/name/role, action (`create`, `update`, `delete`, `import`, `pause`, or `resume`), resource kind and UUID, optional parent RPC-listener UUID, previous/new revisions, and creation time. If the response contains `nextBefore`, set a Postman variable named `nextBefore` to that value and fetch the next page with `GET {{baseUrl}}/api/v1/rpc-listener-audit?limit=50&before={{nextBefore}}`.

## Operational event pagination

`GET /api/v1/operational-events` exposes recent runtime activity without exposing arbitrary log text. RedDotRelay captures only explicitly allowlisted static messages and structured fields; unknown messages and attributes are discarded. The feed retains at most 1,000 entries in process memory and resets on restart, so stdout remains the source for durable centralized logging.

Use `limit` from 1 to 200 and the returned `nextBefore` cursor. Optional `level` values are `info`, `warn`, and `error`; optional `component` values are `server`, `scanner`, and `delivery`. For example:

```http
GET {{baseUrl}}/api/v1/operational-events?limit=50&level=warn&component=delivery
```

## Durable event and delivery diagnostics

`GET /api/v1/events` returns newest-first persisted event summaries with bounded delivery-state counts. It accepts `limit` (1–200), opaque `before`, and exact filters for `chainId`, `transactionHash`, `blockNumber`, `contractAddress`, `eventSignature`, and `deliveryStatus` (`pending`, `delivered`, or `dead`). Pass the returned `nextBefore` unchanged to fetch the next page.

Expand one event with `GET /api/v1/events/{eventId}/deliveries?limit=50`. Each destination has an opaque delivery UUID, current and lifetime attempt counts, last-attempt time, safe HTTP status, next retry, delivered time, and a bounded failure category. Direct URLs are reduced to their origin; referenced destinations remain references. Response bodies, resolved values, credentials, and arbitrary network errors are never returned. Use `nextAfter` unchanged for high-fanout events.

`GET /api/v1/scanner-progress` returns the last observed latest block, confirmed head, durable checkpoint, and confirmed-head lag for each scanner that has contacted its RPC endpoint. This progress is in-memory observation data and starts empty after process restart; the checkpoint itself remains durable in SQLite.

To requeue exactly one dead destination, send `POST /api/v1/deliveries/{deliveryId}/requeue` as an administrator with `Content-Type: application/json` and `{"confirm":true}`. This is an operational action and does not use the configuration ETag. The state change and actor-attributed record are committed in one SQLite transaction. Repeating it after the delivery is no longer dead returns `404`. Review the newest records with `GET /api/v1/delivery-requeue-audit?limit=50` and paginate using its numeric `nextBefore` cursor.

## Automatic delivered-history retention

Automatic retention is disabled when the `retention` YAML block is omitted. To enable it explicitly:

```yaml
retention:
  delivered_for: 720h
  poll_interval: 1h
  batch_size: 1000
```

The worker runs once at startup and then on the configured interval. It permanently removes bounded batches of events only when every destination reached `delivered` before the cutoff. Pending, retrying, leased, and dead deliveries are ineligible. The existing `retention prune` CLI remains available for previewed/manual maintenance.

### Storage and retention operations

The management UI is only an HTTP API client; it never opens SQLite, reads configuration files, or invokes the CLI. `GET /api/v1/storage/status` reports the main database, WAL, SHM, free-page, volume, event, and delivery-state totals. `GET /api/v1/retention/status` reports the configured automatic policy and in-process cleanup state. Admins preview and execute the same shared retention service with `POST /api/v1/retention/preview` and `POST /api/v1/retention/prune`. Pruning and `POST /api/v1/storage/optimize` require an explicit true confirmation property. The CLI remains available for urgent offline administration when the HTTP service is unavailable.

The canonical configuration route is `/api/v1/rpc-listeners`. `/api/v1/rpc-listeners` remains a compatibility alias during the pre-v1 naming migration.

## Common response codes

| Status | Meaning |
| --- | --- |
| `200` | Read or patch succeeded |
| `201` | Resource created; inspect `Location` and `ETag` |
| `204` | Resource deleted; inspect `ETag` |
| `400` | Malformed UUID, JSON, ETag, query, or unknown field |
| `401` | Missing, malformed, invalid, or revoked bearer key |
| `403` | Authenticated key lacks the required role |
| `409` | Configuration contains a direct URL that is unsafe to export |
| `404` | Resource or route was not found |
| `405` | Method is not supported for the route |
| `412` | `If-Match` is stale |
| `415` | Content type is not valid for the operation |
| `422` | The proposed complete configuration is invalid |
| `428` | A mutation omitted `If-Match` |

All responses include `Cache-Control: no-store`, MIME sniffing and frame protections, a restrictive content security policy and permissions policy, same-origin opener isolation, and a no-referrer policy.
