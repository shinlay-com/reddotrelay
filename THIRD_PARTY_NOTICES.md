# Third-party notices

RedDotRelay-owned source code is licensed under AGPL-3.0-only. Dependencies
retain their respective licenses. The version-pinned compiled dependency
inventory is recorded in `LICENSES/go-modules.csv` and
`LICENSES/npm-runtime.csv`; the corresponding license and notice texts are
included below those directories. Each release also carries a machine-readable
SPDX SBOM.

Notable copyleft dependency:

- `github.com/ethereum/go-ethereum`: the imported ABI, RPC client, common, core
  type, and supporting library packages carry LGPL-3.0-or-later headers. The
  upstream repository also contains GPL-3.0 programs that RedDotRelay does not
  import. Both the upstream GPL base text and LGPL additional permissions are
  included in `LICENSES/go/`.

RedDotRelay distributes its complete application source, module checksums, and
build instructions. A recipient may use a `replace` directive in `go.mod` (or a
Go workspace override) to select a modified, interface-compatible go-ethereum
module and rebuild the combined executable with the standard documented build
commands. No signing key or private build material is required.

Other compiled Go modules and the bundled Preact UI runtime use permissive MIT,
BSD, ISC, or Apache-2.0 terms. Their exact versions, declared licenses,
copyright notices, and license texts are included in `LICENSES/`.

Build-only JavaScript tools are recorded in `ui/package-lock.json`. Runtime
base-image and operating-system packages are recorded in the container SPDX
SBOM and retain their upstream terms. Redistributors must preserve this file,
`NOTICE`, `LICENSES/`, and all applicable notices and license texts.
