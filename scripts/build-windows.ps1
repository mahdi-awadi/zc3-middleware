# Builds the Windows desktop app (Electron shell + Go server sidecar).
#   scripts\build-windows.ps1            -> desktop\release\GymGate-<ver>-x64.exe (installer)
#                                           desktop\release\GymGate-<ver>-portable.exe
#   scripts\build-windows.ps1 -Sidecar   -> only rebuild desktop\bin\middleware.exe (for `npm start`)
param([string]$Version = "", [switch]$Sidecar)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# Go / Node may be installed but not yet on this shell's PATH.
foreach ($p in @("$env:ProgramFiles\Go\bin", "$env:ProgramFiles\nodejs")) {
    if ((Test-Path $p) -and ($env:Path -notlike "*$p*")) { $env:Path = "$p;$env:Path" }
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw "Go not found (winget install GoLang.Go), then reopen the terminal." }
if (-not (Get-Command npm -ErrorAction SilentlyContinue)) { throw "Node.js/npm not found (winget install OpenJS.NodeJS.LTS)." }

if (-not $Version) {
    try { $Version = (git describe --tags --always --dirty 2>$null) } catch {}
    if (-not $Version) { $Version = "dev" }
}

# 1. Go server sidecar (headless build, embedded console).
New-Item -ItemType Directory -Force desktop\bin | Out-Null
$env:CGO_ENABLED = "0"; $env:GOOS = "windows"; $env:GOARCH = "amd64"
go build -trimpath -ldflags "-s -w -X main.version=$Version" -o desktop\bin\middleware.exe .\cmd\middleware
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host "built desktop\bin\middleware.exe ($Version)"
if ($Sidecar) { exit 0 }

# 2. Electron package.
Push-Location desktop
try {
    if (-not (Test-Path node_modules)) { npm install; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE } }
    npm run dist
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally { Pop-Location }

Get-ChildItem desktop\release\*.exe | ForEach-Object { Write-Host ("built {0} ({1:N1} MB)" -f $_.FullName, ($_.Length / 1MB)) }
