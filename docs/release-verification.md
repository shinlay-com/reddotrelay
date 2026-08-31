# Release artifact verification

Never deploy a release based only on a matching filename or tag. Verify the
checksum, keyless signing identity, build provenance, and container digest from
a trusted checkout of the public repository.

The workflow path is part of the trust policy and must match exactly.

## Portable archive

Download the archive, `checksums.txt`, `checksums.txt.sig`, and
`checksums.txt.pem` from the same release. Verify the checksum manifest's
keyless signature:

```text
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig \
  --certificate checksums.txt.pem \
  --certificate-identity-regexp '^https://github.com/shinlay-com/reddotrelay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Then compare the archive's SHA-256 digest with its line in `checksums.txt`. On
Windows, use `Get-FileHash -Algorithm SHA256 <archive>`; on Linux or macOS, use
`sha256sum <archive>` or `shasum -a 256 <archive>`.

Verify GitHub provenance after authenticating the GitHub CLI:

```text
gh attestation verify <archive> --repo shinlay-com/reddotrelay
```

Extract the archive, run `reddotrelay -version` (or `reddotrelay.exe -version`),
and confirm it matches the release without a `dev` or snapshot suffix.

## Container image

Resolve and record the immutable digest; do not deploy the mutable `latest` tag.
Verify the keyless signature and GitHub provenance:

```text
cosign verify \
  --certificate-identity-regexp '^https://github.com/shinlay-com/reddotrelay/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  docker.io/shinlay/reddotrelay@sha256:<digest>

gh attestation verify oci://docker.io/shinlay/reddotrelay@sha256:<digest> \
  --repo shinlay-com/reddotrelay
```

Inspect the attached SBOM/provenance and repeat the high/critical vulnerability
scan before promoting into a controlled environment. Confirm that `/licenses`
inside the image contains the AGPL text, `NOTICE`, third-party notices, and the
version-pinned `LICENSES/` bundle.

## Failure handling

Do not deploy when identity, provenance, checksum, SBOM, version, or digest
verification fails. Retain the evidence, report ordinary packaging defects in a
public issue without secrets, and report suspected compromise privately under
`SECURITY.md`.
