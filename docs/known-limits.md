# Engine v1 known limits

RedDotRelay Engine v1 supports one active process and one local SQLite database
on a durable filesystem. The following are unsupported or intentionally absent:

- Multiple active writers, horizontal replicas sharing a database, and SQLite
  on NFS/SMB or another filesystem without reliable local locking and fsync.
- PostgreSQL, Redis, Kafka, an external queue, multi-tenant identity, billing,
  fleet orchestration, or managed secret storage inside the Engine.
- WebSocket subscriptions as a correctness path, unconfirmed-event delivery,
  compensating webhook notifications for reorgs, or exactly-once delivery.
- EVM transaction signing, private-key custody, chain writes, or wallet behavior.
- Automatic rollback of forward SQLite migrations. Rollback requires restoring
  the matching pre-upgrade backup with the older image.
- Live RPC environment-variable rotation; RPC references are resolved when a
  scanner runtime is built. Mounted webhook URLs and HMAC files are resolved on
  each delivery attempt.
- Durable operational-event logs. The bounded operational feed resets on
  process replacement; stdout must be shipped externally for durable logs.
  Browser sessions are persisted as token hashes in SQLite but still expire
  after 30 minutes idle or eight hours absolute.
- A public management listener without an operator-provided trusted HTTPS and
  network boundary. RedDotRelay does not include self-service password recovery,
  MFA, or external identity providers. The Engine provides bounded account-based
  sign-in throttling, but the trusted reverse proxy remains responsible for
  client-IP rate limiting and abuse controls.

The validated sizing floor is at least 3 events/second for five minutes under
the hardware and local-latency conditions in `reliability-testing.md`; it is not
a maximum or SLA. Payload size, destination count, RPC behavior, webhook
latency, confirmation depth, retention, disk speed, and host limits materially
change capacity. Linux amd64 is Tier 1. Linux arm64 requires continuous CI
exercise before Tier 1 promotion; native Windows/macOS archives are convenience
artifacts, not production tiers.

Deep reorgs into pruned history may redeliver the same deterministic event ID.
Webhook consumers must always deduplicate. A receiver acceptance followed by an
Engine crash before completion persistence can also cause a retry by design.
