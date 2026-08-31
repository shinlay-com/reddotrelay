# Rebuilding with a modified go-ethereum library

RedDotRelay uses go-ethereum library packages licensed LGPL-3.0-or-later. A
release includes complete RedDotRelay source, `go.mod`, `go.sum`, build scripts,
and dependency notices. No private compiler, signing key, or unpublished build
material is required to rebuild the executable.

## Standard build

Install the Go version declared in `go.mod` and the Node.js version documented
in `README.md`, then run:

```powershell
npm --prefix ui ci
npm --prefix ui run check
npm --prefix ui run build
go test ./...
go vet ./...
go build ./...
```

The release container uses the same source build in `Dockerfile`.

## Select a modified library

Clone go-ethereum at the version recorded in `go.mod`, apply the desired
modification, and point the RedDotRelay build at that working tree. For example:

```powershell
go mod edit -replace github.com/ethereum/go-ethereum=C:\source\go-ethereum
go mod tidy
go test ./...
go build ./cmd/reddotrelay
```

On Linux or macOS, use the appropriate absolute filesystem path. A Go workspace
may be used instead of editing `go.mod`:

```text
go work init . /absolute/path/to/go-ethereum
go work use . /absolute/path/to/go-ethereum
go build ./cmd/reddotrelay
```

Do not commit a local `replace` directive or `go.work` file to an upstream
RedDotRelay change. Modified go-ethereum versions remain subject to their
upstream license terms.

## Verify the result

Run the complete validation suite and print the resulting Engine version:

```powershell
./scripts/validate.ps1
./reddotrelay.exe -version
```

The output binary can then be substituted in a local container image built from
an equivalent Dockerfile. Recipients are not required to use an official
RedDotRelay signature for their modified builds and must not present modified
artifacts as official Shinlay releases.
