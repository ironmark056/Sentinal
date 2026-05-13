# Install the latest sentinel release for Windows.
#
# Usage (run in PowerShell):
#   iwr -useb https://raw.githubusercontent.com/your-org/sentinel/main/scripts/install.ps1 | iex
#
# Env vars:
#   $env:SENTINEL_VERSION  pin a specific version (e.g. v0.1.0); default: latest
#   $env:SENTINEL_BIN_DIR  install prefix; default: $env:LOCALAPPDATA\Programs\sentinel
#   $env:SENTINEL_REPO     GitHub repo; default: your-org/sentinel

$ErrorActionPreference = "Stop"

$repo    = if ($env:SENTINEL_REPO)    { $env:SENTINEL_REPO }    else { "your-org/sentinel" }
$version = if ($env:SENTINEL_VERSION) { $env:SENTINEL_VERSION } else { "latest" }
$binDir  = if ($env:SENTINEL_BIN_DIR) { $env:SENTINEL_BIN_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\sentinel" }

# Detect arch.
switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { $arch = "amd64" }
    "ARM64" { $arch = "arm64" }
    default { throw "Unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)" }
}

if ($version -eq "latest") {
    $url = "https://github.com/$repo/releases/latest/download/sentinel-windows-$arch.exe"
} else {
    $url = "https://github.com/$repo/releases/download/$version/sentinel-windows-$arch.exe"
}

New-Item -ItemType Directory -Force -Path $binDir | Out-Null
$target = Join-Path $binDir "sentinel.exe"

Write-Host "Downloading $url"
Invoke-WebRequest -Uri $url -OutFile $target -UseBasicParsing

Write-Host "Installed: $target"
Write-Host ""
Write-Host "Add $binDir to your PATH if it's not already there:"
Write-Host "  [Environment]::SetEnvironmentVariable('Path', `"`$(([Environment]::GetEnvironmentVariable('Path','User'))`);$binDir`", 'User')"
Write-Host ""
Write-Host "Then:"
Write-Host "  sentinel init       # writes %APPDATA%\sentinel\sentinel.yaml"
Write-Host "  sentinel dashboard  # opens http://127.0.0.1:7842"
