# Engine compatibility and support policy

RedDotRelay uses semantic versioning. Before v1, minor releases may change
unstable contracts with release-note notice and a migration path. Starting with
v1, documented management API paths and schemas, configuration export schema,
webhook envelope and authentication contract, SQLite migrations, CLI operations,
metrics, and supported static YAML keys follow the compatibility guarantees in
`engine-v1-scope.md`.

Patch releases fix defects without intentional contract changes. Minor releases
may add optional fields, endpoints, metrics, and behavior while preserving
existing clients. Incompatible changes require a major release, except when
retaining behavior would expose a security or data-loss defect; those exceptions
must be documented prominently with mitigation and migration instructions.

The latest minor line receives fixes. After v1, the immediately previous minor
line receives critical security fixes for at least 90 days after its successor
unless a release note announces a longer window. Pre-v1 supports only the latest
release.

Supported production shape: one active RedDotRelay process writing one local
SQLite database on a durable filesystem. Active/passive replacement may move
that database between containers, but concurrent writers, shared network
filesystems, multi-primary operation, PostgreSQL, and horizontal replicas over
one database are unsupported.
