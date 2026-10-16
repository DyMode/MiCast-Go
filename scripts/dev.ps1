param([string]$Listen = '127.0.0.1:42310', [string]$DataDir = 'data', [switch]$ShowAccessKey)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path $PSScriptRoot -Parent
Set-Location $projectRoot
$goExe = Join-Path $projectRoot '.tools\go\bin\go.exe'
if (-not (Test-Path -LiteralPath $goExe)) { $goExe = (Get-Command go -ErrorAction Stop).Source }
$runArgs = @('run', './cmd/micast-go', '--listen', $Listen, '--data-dir', $DataDir)
if ($ShowAccessKey) { $runArgs += '--show-access-key' }
& $goExe @runArgs
exit $LASTEXITCODE
