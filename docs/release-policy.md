# Release policy

## Version and source

Releases are built only from an annotated `vMAJOR.MINOR.PATCH` tag whose version
matches the release title. The tag must point to a reviewed commit on the default
branch, and the working tree must be clean. Release notes describe behavior,
migrations, security fixes, known limits, and rollback requirements.

## Required gates

1. Run the complete validation suite and `git diff --check`.
2. Repeat Gitleaks, govulncheck, npm audit, Trivy, and
   `scripts/check-third-party-licenses.ps1` against the release commit and final
   image.
3. Run the documented container-replacement and real-EVM release-candidate tests.
4. Build archives and the image from the tag in GitHub Actions.
5. Publish SHA-256 checksums, SPDX SBOMs, GitHub build provenance, and keyless
   Sigstore signatures for checksums and the image digest.
6. Verify the downloaded artifacts and a clean-container startup before marking
   the release supported.

Consumer verification commands are maintained in
[`release-verification.md`](release-verification.md).

GitHub OIDC is the signing identity; no long-lived signing key belongs in the
repository. Consumers must verify the certificate identity against the release
workflow path and the repository's final public URL, not merely check that a
signature is mathematically valid.

Release-tool versions are pinned in the workflow and updated through reviewed
dependency changes. The validated baseline is GoReleaser v2.18.0, Syft v1.51.0,
Trivy v0.74.0, and Gitleaks v8.30.1.

## Artifacts

The container is the primary single-image distribution. Portable archives
contain the platform binary, built management UI, example archive configuration,
the complete `LICENSES/` bundle, notices, OpenAPI contract, and documentation.
The tagged public source and exact `go.mod`/`go.sum` are required to support
dependency review and LGPL rebuilding.

## Rollback

Create and verify a SQLite backup before upgrading. Database migrations are
forward-only. Rollback means stopping the new version, restoring its pre-upgrade
backup, and starting the recorded older digest. Never run an older binary against
a database already migrated by a newer release.

## Revocation

Do not silently replace release files or move tags. If an artifact is defective,
mark the release unsupported, publish an advisory, and issue a new version. A
compromised signing identity or artifact requires immediate release withdrawal
and keyless-identity/provenance investigation.
