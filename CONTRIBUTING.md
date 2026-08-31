# Contributing to RedDotRelay

Thank you for your interest in improving RedDotRelay Engine. Issues, design
feedback, documentation corrections, and security reports are welcome.

RedDotRelay is not currently accepting external code contributions. This
temporary policy preserves clear copyright ownership while the contributor
licensing process is established. Do not submit source-code pull requests until
the maintainers publish a contributor agreement and reopen code contributions.

## Before opening a change

- Use a discussion or issue for a new public contract, architecture change, or
  substantial feature. Report vulnerabilities only through `SECURITY.md`.
- Keep the Engine single-process, SQLite-only, and independently operable.
  Multi-tenancy, billing, fleet provisioning, and PostgreSQL control-plane work
  are outside this repository's Engine scope.
- Add correctness-focused tests for persistence, reorg, retry, authorization,
  secret handling, and compatibility changes.
- Update OpenAPI and operator documentation when behavior changes.

## Development

Install Go 1.25+, Node.js 22+, and Docker when testing the image. Run:

```text
./scripts/validate.ps1
git diff --check
```

Maintainer changes must be focused, explain user-visible behavior and risks,
list verification performed, and call out migrations or compatibility effects.
Generated UI output, local databases, credentials, and scan artifacts must not
be committed.

## Review requirements

At least one maintainer approval and passing required checks are required.
Changes to persistence, authentication, secret handling, release workflows, or
the v1 compatibility contract require a second maintainer or an explicitly
recorded owner decision. Authors must not merge their own security-sensitive
change without independent review.

Community participation must follow `CODE_OF_CONDUCT.md`. All changes must
preserve third-party license notices.
