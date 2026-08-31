# SQLite backup, restore, upgrade, and rollback

RedDotRelay Engine uses one SQLite database as its complete durable state. A configuration JSON export is useful for moving desired configuration, but it is not a database backup: it excludes API keys, checkpoints, events, deliveries, and audits.

Use the built-in database commands for release upgrades and disaster recovery. They verify each backup with SQLite `quick_check`. Backup uses SQLite `VACUUM INTO`, so it produces a transactionally consistent snapshot while RedDotRelay is running and WAL mode is active.

## Create and verify a backup

Choose a new filename. RedDotRelay refuses to overwrite an existing backup.

```text
docker compose exec reddotrelay /reddotrelay database backup -config /etc/reddotrelay/config.yaml -output /var/lib/reddotrelay/reddotrelay-before-upgrade.db
```

The success message begins with `created verified SQLite backup`. Keep a second copy outside the Docker volume. Get the container ID with `docker compose ps -q reddotrelay`, then replace `<container-id>` below:

```text
docker cp <container-id>:/var/lib/reddotrelay/reddotrelay-before-upgrade.db ./reddotrelay-before-upgrade.db
```

Do not copy the live `reddotrelay.db` file directly. Copying a WAL-mode database without its transaction state can produce an incomplete backup.

## Upgrade an image

1. Create the verified backup and copy it outside the volume.
2. Record the current image tag or digest.
3. Rebuild or pull the intended image.
4. Replace the container while preserving the `reddotrelay-data` volume.
5. Verify `/healthz`, `/readyz`, `/metrics`, scanner progress, and delivery counts.
6. Verify that existing API keys still work. An image replacement does not require a new `api_key_` key when the volume is preserved.

For a local source build:

```text
docker compose up --build --force-recreate -d
docker compose ps
docker compose logs reddotrelay
```

Opening an older supported database performs forward migrations automatically. Startup stops on migration failure. Do not attempt to use the upgraded database with an older image.

## Restore or roll back

Restore is deliberately offline. Stop the service before running it; the confirmation flag is an assertion that no RedDotRelay process is using the configured database.

```text
docker compose stop reddotrelay
docker compose run --rm reddotrelay database restore -config /etc/reddotrelay/config.yaml -input /var/lib/reddotrelay/reddotrelay-before-upgrade.db -confirm-service-stopped
docker compose up -d reddotrelay
```

Restore verifies the input, stages and syncs it in the durable directory, replaces the database atomically, and removes stale `-wal` and `-shm` sidecars. It refuses to use the live database itself as the backup.

For rollback, restore the pre-upgrade backup first and then start the recorded older image. Never start the older image against a database already migrated by a newer release.

After restore, verify:

- `/healthz` and `/readyz` return success.
- Existing API keys authenticate.
- The expected RPC listener revision and RPC listener list are present.
- Scanner checkpoints and progress match the backup time.
- Pending and dead deliveries are present and resume according to their stored state.
- Configuration and delivery audit history is intact.

## Recovery cautions

- The database commands operate only on files visible inside the same container or host environment.
- A restore intentionally discards durable changes made after the backup timestamp.
- Preserve the external backup until the upgraded deployment has passed its operational checks.
- Monitor free disk space: `VACUUM INTO` and restore staging require space for another complete database file.
- Keep exactly one active RedDotRelay writer for a database and its volume.
