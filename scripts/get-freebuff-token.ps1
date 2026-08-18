# get-freebuff-token.ps1 - Legacy alias forwarding to gen-token.ps1
param(
    [switch]$Save,
    [switch]$ToClipboard,
    [switch]$Incognito,
    [switch]$Append,
    [string]$Browser = "auto",
    [string]$EnvFile = "",
    [string]$BaseUrl = $(if ($env:FREEBUFF_BASE_URL) { $env:FREEBUFF_BASE_URL } else { "https://www.codebuff.com" }),
    [int]$TimeoutSeconds = 300,
    [int]$PollIntervalMs = 5000,
    [switch]$NoHealthCheck,
    [switch]$Help
)

$scriptPath = Join-Path $PSScriptRoot "gen-token.ps1"
if (-not (Test-Path $scriptPath)) {
    $scriptPath = Join-Path $PSScriptRoot "gen-freebuff-token.ps1"
}
& $scriptPath @PSBoundParameters
