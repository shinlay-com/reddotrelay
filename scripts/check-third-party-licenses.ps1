$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$licenseRoot = Join-Path $repositoryRoot 'LICENSES'
$goManifestPath = Join-Path $licenseRoot 'go-modules.csv'
$npmManifestPath = Join-Path $licenseRoot 'npm-runtime.csv'

$env:GOCACHE = Join-Path $repositoryRoot '.cache\license-go-build'
$env:GOPROXY = 'off'

Push-Location $repositoryRoot
$originalGOOS = $env:GOOS
$originalGOARCH = $env:GOARCH
try {
    $releaseTargets = @(
        @{ GOOS = 'linux'; GOARCH = 'amd64' },
        @{ GOOS = 'linux'; GOARCH = 'arm64' },
        @{ GOOS = 'windows'; GOARCH = 'amd64' },
        @{ GOOS = 'windows'; GOARCH = 'arm64' },
        @{ GOOS = 'darwin'; GOARCH = 'amd64' },
        @{ GOOS = 'darwin'; GOARCH = 'arm64' }
    )
    $actualModules = foreach ($target in $releaseTargets) {
        $env:GOOS = $target.GOOS
        $env:GOARCH = $target.GOARCH
        $targetModules = & go list -deps -f '{{with .Module}}{{if ne .Path "reddotrelay"}}{{.Path}}|{{.Version}}{{end}}{{end}}' ./cmd/reddotrelay
        if ($LASTEXITCODE -ne 0) {
            throw "Unable to enumerate modules compiled into RedDotRelay for $($target.GOOS)/$($target.GOARCH)."
        }
        $targetModules
    }
    $actualModules = $actualModules |
        Where-Object { -not [string]::IsNullOrWhiteSpace($_) } |
        Sort-Object -Unique
} finally {
    $env:GOOS = $originalGOOS
    $env:GOARCH = $originalGOARCH
    Pop-Location
}

$goManifest = Import-Csv -LiteralPath $goManifestPath
$expectedModules = $goManifest |
    ForEach-Object { "$($_.Module)|$($_.Version)" } |
    Sort-Object -Unique

$moduleDifference = Compare-Object -ReferenceObject $expectedModules -DifferenceObject $actualModules
if ($moduleDifference) {
    $moduleDifference | Format-Table | Out-String | Write-Error
    throw 'Compiled Go dependency set differs from LICENSES/go-modules.csv.'
}

foreach ($entry in $goManifest) {
    foreach ($licenseFile in ($entry.LicenseFiles -split ';')) {
        $path = Join-Path (Join-Path $licenseRoot 'go') $licenseFile
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            throw "Missing Go dependency license file: $path"
        }
        if ((Get-Item -LiteralPath $path).Length -eq 0) {
            throw "Empty Go dependency license file: $path"
        }
    }
}

$packageLock = Get-Content -LiteralPath (Join-Path $repositoryRoot 'ui\package-lock.json') -Raw | ConvertFrom-Json -AsHashtable
$npmManifest = Import-Csv -LiteralPath $npmManifestPath
foreach ($entry in $npmManifest) {
    $lockKey = "node_modules/$($entry.Package)"
    $lockedPackage = $packageLock.packages[$lockKey]
    if ($null -eq $lockedPackage) {
        throw "Runtime UI dependency is absent from package-lock.json: $($entry.Package)"
    }
    if ($lockedPackage.version -ne $entry.Version) {
        throw "Runtime UI dependency version differs for $($entry.Package): expected $($entry.Version), found $($lockedPackage.version)"
    }
    if ($lockedPackage.license -ne $entry.DeclaredLicense) {
        throw "Runtime UI dependency license differs for $($entry.Package): expected $($entry.DeclaredLicense), found $($lockedPackage.license)"
    }
    $licensePath = Join-Path $licenseRoot $entry.LicenseFile
    if (-not (Test-Path -LiteralPath $licensePath -PathType Leaf)) {
        throw "Missing UI dependency license file: $licensePath"
    }
}

Write-Output "Verified $($goManifest.Count) release-target Go modules and $($npmManifest.Count) bundled UI runtime dependency license records."
