# start-proxy.ps1 - Launch freebuff-proxy from the extracted folder.
# Right-click this folder -> "Open in Terminal" -> .\start-proxy.ps1
$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root
$exe = Join-Path $root "freebuff-proxy.exe"

if (-not (Test-Path $exe)) {
    Write-Host "freebuff-proxy.exe not found next to this script." -ForegroundColor Red
    exit 1
}
if (-not (Test-Path (Join-Path $root ".env"))) {
    if (Test-Path (Join-Path $root ".env.example")) {
        Copy-Item (Join-Path $root ".env.example") (Join-Path $root ".env")
        Write-Host "No .env found; created it from .env.example" -ForegroundColor Yellow
    }
}

Write-Host "Starting freebuff-proxy from $root" -ForegroundColor Cyan
& $exe
$code = $LASTEXITCODE
if ($code -ne 0) {
    Write-Host "freebuff-proxy exited with code $code" -ForegroundColor Red
}
exit $code
