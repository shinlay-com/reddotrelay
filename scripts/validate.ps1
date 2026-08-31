$ErrorActionPreference = "Stop"

$go = Get-Command go -ErrorAction SilentlyContinue
if ($null -eq $go) {
    $installedGo = "C:\Program Files\Go\bin\go.exe"
    if (-not (Test-Path -LiteralPath $installedGo)) {
        throw "Go was not found. Install Go 1.25 or add its bin directory to PATH."
    }
    $goPath = $installedGo
    $gofmtPath = "C:\Program Files\Go\bin\gofmt.exe"
} else {
    $goPath = $go.Source
    $gofmtPath = Join-Path (Split-Path -Parent $goPath) "gofmt.exe"
    if (-not (Test-Path -LiteralPath $gofmtPath)) {
        $gofmtPath = "gofmt"
    }
}

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$env:GOCACHE = Join-Path $repositoryRoot ".cache\go-build"
$env:npm_config_cache = Join-Path $repositoryRoot ".cache\npm"
$uiRoot = Join-Path $repositoryRoot "ui"
$npm = Get-Command npm -ErrorAction SilentlyContinue
if ($null -eq $npm) {
    throw "npm was not found. Install Node.js 22 or add npm to PATH."
}

Push-Location $uiRoot
try {
    & $npm.Source ci
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    & $npm.Source run check
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    & $npm.Source run build
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
    Pop-Location
}

$goFiles = Get-ChildItem -LiteralPath $repositoryRoot -Recurse -Filter '*.go' -File |
    Where-Object { $_.FullName -notmatch '[\\/]\.cache[\\/]' } |
    Select-Object -ExpandProperty FullName

foreach ($goFile in $goFiles) {
    & $gofmtPath -w $goFile
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

Push-Location $repositoryRoot
try {
    & $goPath mod download
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    & (Join-Path $PSScriptRoot "check-third-party-licenses.ps1")
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    & $goPath test ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    & $goPath vet ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    & $goPath build ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
    Pop-Location
}
