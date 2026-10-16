$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path $PSScriptRoot -Parent
Set-Location $projectRoot
$goExe = Join-Path $projectRoot '.tools\go\bin\go.exe'
if (-not (Test-Path -LiteralPath $goExe)) { $goExe = (Get-Command go -ErrorAction Stop).Source }
New-Item -ItemType Directory -Force dist | Out-Null
$previousOS = $env:GOOS
$previousArch = $env:GOARCH
$previousCGO = $env:CGO_ENABLED
try {
    $env:CGO_ENABLED = '0'
    foreach ($platform in @(@{OS='windows'; Arch='amd64'; Suffix='.exe'}, @{OS='linux'; Arch='amd64'; Suffix=''}, @{OS='linux'; Arch='arm64'; Suffix=''})) {
        $env:GOOS = $platform.OS
        $env:GOARCH = $platform.Arch
        $target = 'dist/micast-go-' + $platform.OS + '-' + $platform.Arch + $platform.Suffix
        & $goExe build -trimpath -ldflags '-s -w' -o $target ./cmd/micast-go
        if ($LASTEXITCODE -ne 0) { throw "Build failed: $target" }
        Get-Item -LiteralPath $target | Select-Object Name,Length
    }
} finally {
    $env:GOOS = $previousOS
    $env:GOARCH = $previousArch
    $env:CGO_ENABLED = $previousCGO
}
