# get-freebuff-token.ps1 - legacy alias for gen-freebuff-token.ps1
# Generates a FreeBuff auth token via zero-dependency headless OAuth login.
param(
    [switch]$Save,
    [switch]$ToClipboard,
    [switch]$Append,
    [string]$EnvFile = "",
    [switch]$Print,
    [switch]$Force,
    [switch]$Logout
)

$scriptDir = $PSScriptRoot
$genScript = Join-Path $scriptDir "gen-freebuff-token.ps1"
if (-not (Test-Path $genScript)) {
    $genScript = Join-Path $scriptDir "gen-token.ps1"
}

$params = @{}
if ($Save) { $params['Save'] = $true }
if ($ToClipboard) { $params['ToClipboard'] = $true }
if ($Append) { $params['Append'] = $true }
if ($EnvFile) { $params['EnvFile'] = $EnvFile }

& $genScript @params
