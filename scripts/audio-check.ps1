param([string]$FFmpeg = 'ffmpeg')
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path $PSScriptRoot -Parent
Set-Location $projectRoot
$goExe = Join-Path $projectRoot '.tools\go\bin\go.exe'
if (-not (Test-Path -LiteralPath $goExe)) { $goExe = (Get-Command go -ErrorAction Stop).Source }
& $goExe run ./cmd/audio-check -ffmpeg $FFmpeg
exit $LASTEXITCODE
