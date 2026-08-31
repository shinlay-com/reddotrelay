# RedDotRelay

RedDotRelay is a secure, self-hosted EVM event-to-webhook relay. It scans confirmed chain logs, decodes ABI events, stores state in SQLite, and delivers webhooks with durable retries in one process and one container.

RedDotRelay is open source under [AGPL-3.0-only](LICENSE). See [third-party notices](THIRD_PARTY_NOTICES.md), [security policy](SECURITY.md), and [support](SUPPORT.md).

## Quick start

```sh
docker compose up --build -d
```

Open <http://127.0.0.1:8080/ui/> and create the initial administrator.

To test the published Docker Hub image instead of building locally:

```powershell
docker pull shinlay/reddotrelay:latest
docker volume create reddotrelay-data
docker run --rm --name reddotrelay `
  -p 8080:8080 `
  -v reddotrelay-data:/var/lib/reddotrelay `
  shinlay/reddotrelay:latest
```

Check <http://127.0.0.1:8080/healthz>, then open
<http://127.0.0.1:8080/ui/> to create the initial administrator.

For a local source build, install Go 1.25+ and Node.js 24+, then run:

```powershell
Copy-Item config.example.yaml config.yaml
npm --prefix ui ci
npm --prefix ui run build
go run ./cmd/reddotrelay -config config.yaml
```

## Features

- Confirmed `eth_getLogs` scanning with durable checkpoints and reorg recovery.
- ABI event decoding, deterministic event IDs, and SQLite persistence.
- At-least-once webhook delivery with retries and dead letters.
- Embedded management UI and authenticated REST API.
- RPC authentication: HTTP Basic, static Bearer, API key, or provider JWT.

YAML contains static process settings. Listener, contract, webhook, user, API key, retention, and audit configuration is managed through the authenticated UI or API and stored in SQLite. Keep credential-bearing values in environment or file references; never commit secrets.

## API and operations

The service listens on port 8080. `/healthz`, `/readyz`, and `/metrics` expose health and telemetry. Management routes under `/api/v1/` require a UI session or API key.

Create an admin API key locally:

```powershell
go run ./cmd/reddotrelay api-key create -config config.yaml -name operations -role admin
```

Send it as `Authorization: Bearer <secret>`. Secrets are shown only once and stored as hashes.

## License and support

Modified network versions must provide corresponding source as required by the AGPL. Dependency licenses are included under `LICENSES/`.

Report security issues privately using [SECURITY.md](SECURITY.md). Use the repository GitHub issue tracker for bugs, questions, and feature requests.
