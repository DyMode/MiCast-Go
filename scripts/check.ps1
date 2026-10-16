$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path $PSScriptRoot -Parent
Set-Location $projectRoot
$goExe = Join-Path $projectRoot '.tools\go\bin\go.exe'
if (-not (Test-Path -LiteralPath $goExe)) { $goExe = (Get-Command go -ErrorAction Stop).Source }
node --test scripts/qr-flow.test.cjs
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
node --check internal/server/web/support.js
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
python scripts/sync-legal.py --check
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
python scripts/fnos-lifecycle.test.py
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
& $goExe vet ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
& $goExe test ./... -count=1
exit $LASTEXITCODE
