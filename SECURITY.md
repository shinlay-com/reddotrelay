# RedDotRelay security policy

## Reporting a vulnerability

Report suspected vulnerabilities privately through this repository's GitHub
Security tab by opening a private security advisory. Do not include secrets,
customer data, exploit details, or an unpatched vulnerability in a public issue
or discussion.

Include the affected version or commit, the deployment shape, reproduction
steps, expected impact, and any suggested mitigation. Use synthetic credentials
and data in reproductions.

Maintainers will acknowledge the report privately, validate its scope, and
coordinate remediation and disclosure with the reporter. A public advisory and
fixed release will be prepared after a mitigation is available. Response and
release timing depend on severity and investigation complexity; this policy
does not promise a fixed service-level agreement.

Repository owners must enable GitHub private vulnerability reporting before a
public release. Until this repository has a private reporting channel, do not
publish it as a supported production release.

## Supported versions

Before v1.0.0, only the latest released version is eligible for security fixes.
The v1 support window will be published with the v1 release policy.

## Operator responsibilities

- Keep management API keys, RPC credentials, webhook URLs containing tokens,
  and HMAC keys in an environment or mounted secret file.
- Expose the management API only through authenticated, TLS-protected access.
- Use secure UI cookies whenever TLS terminates at RedDotRelay, and ensure a
  trusted proxy overwrites forwarded headers when TLS terminates upstream.
- Rotate a credential immediately if it may have appeared in logs, source
  control, configuration exports, or support material.
- Apply security releases promptly and retain verified SQLite backups before an
  upgrade.

## Security boundaries

RedDotRelay is an event relay, not a wallet or signer. It must not be given EVM
private keys. Webhook delivery is at-least-once; receivers must authenticate
requests, enforce timestamp freshness, and deduplicate by event ID.
