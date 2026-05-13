# Build sentinel + sentinel-server for every supported platform.
# Output: dist\<pkg>-<os>-<arch>[.exe]
#
# Usage:
#   .\scripts\build-release.ps1                # all platforms, both binaries
#   .\scripts\build-release.ps1 linux/amd64    # one platform, both binaries

param([string[]]$Targets = @())

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

try {
    $version = git describe --tags --always --dirty 2>$null
    if ([string]::IsNullOrEmpty($version)) { $version = "dev" }
} catch {
    $version = "dev"
}
if ($env:SENTINEL_VERSION) { $version = $env:SENTINEL_VERSION }

$ldflags = "-s -w -X main.version=$version"
$packages = @("sentinel", "sentinel-server")

if ($Targets.Count -eq 0) {
    $Targets = @(
        "linux/amd64",
        "linux/arm64",
        "darwin/amd64",
        "darwin/arm64",
        "windows/amd64",
        "windows/arm64"
    )
}

New-Item -ItemType Directory -Force -Path dist | Out-Null
Write-Host "Building sentinel + sentinel-server $version"
Write-Host ""

foreach ($t in $Targets) {
    $parts = $t.Split("/")
    $os = $parts[0]
    $arch = $parts[1]
    foreach ($pkg in $packages) {
        $out = "dist\$pkg-$os-$arch"
        if ($os -eq "windows") { $out = "$out.exe" }

        Write-Host "  -> $out"
        $env:GOOS = $os
        $env:GOARCH = $arch
        $env:CGO_ENABLED = "0"
        & go build -trimpath -ldflags $ldflags -o $out "./cmd/$pkg"
        if ($LASTEXITCODE -ne 0) {
            Write-Error "Build failed for $t ($pkg)"
            exit 1
        }
    }
}

# Clean up env vars set by the loop.
Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue

Write-Host ""
Write-Host "Done. Outputs:"
Get-ChildItem dist\sentinel-*, dist\sentinel-server-* | Format-Table Name, Length, LastWriteTime
