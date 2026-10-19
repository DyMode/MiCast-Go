param([ValidateSet('all','x86','arm')][string]$Platform='all', [string]$Fnpack='')
$ErrorActionPreference='Stop'
$projectRoot=Split-Path $PSScriptRoot -Parent
Set-Location $projectRoot
$goExe=Join-Path $projectRoot '.tools\go\bin\go.exe'
if (-not (Test-Path -LiteralPath $goExe)) { $goExe=(Get-Command go -ErrorAction Stop).Source }
if (-not $Fnpack) {
    $Fnpack=Join-Path $projectRoot '.tools\fnpack.exe'
    if (-not (Test-Path -LiteralPath $Fnpack)) { $Fnpack=Join-Path (Split-Path $projectRoot -Parent) 'MiCast\build-tools\fnpack.exe' }
    if (-not (Test-Path -LiteralPath $Fnpack)) { $Fnpack=(Get-Command fnpack -ErrorAction Stop).Source }
}
$Fnpack=(Resolve-Path -LiteralPath $Fnpack).Path
& (Join-Path $PSScriptRoot 'build.ps1')
if ($LASTEXITCODE -ne 0) { throw 'Service build failed' }
& $goExe build -trimpath -o .tools/fpk-tool.exe ./cmd/fpk-tool
if ($LASTEXITCODE -ne 0) { throw 'FPK verification tool build failed' }
$version=([regex]::Match((Get-Content packaging/fnos/manifest -Raw),'(?m)^version=(.+)$')).Groups[1].Value.Trim()
$locks=Get-Content packaging/fnos/audio-runtime-lock.json -Raw | ConvertFrom-Json
$moduleLicenses=Join-Path $projectRoot 'licenses\go-modules'
New-Item -ItemType Directory -Force $moduleLicenses | Out-Null
$modules=& $goExe list -m -f '{{.Path}}|{{.Version}}|{{.Dir}}' all
if ($LASTEXITCODE -ne 0) { throw 'Cannot inspect module licenses' }
foreach ($module in $modules) {
    $parts=$module.Split('|')
    if ($parts.Length -lt 3 -or -not $parts[1] -or -not $parts[2]) { continue }
    $license=Get-ChildItem -LiteralPath $parts[2] -File | Where-Object { $_.Name -match '^(LICENSE|COPYING|NOTICE)(\..*)?$' }
    $moduleDirectory=Join-Path $moduleLicenses ($parts[0].Replace('/','_'))
    New-Item -ItemType Directory -Force $moduleDirectory | Out-Null
    foreach ($item in $license) { Copy-Item -LiteralPath $item.FullName -Destination (Join-Path $moduleDirectory $item.Name) -Force }
}
$goroot=& $goExe env GOROOT
Copy-Item -LiteralPath (Join-Path $goroot 'LICENSE') -Destination (Join-Path $moduleLicenses 'Go-LICENSE')
$platforms=@('x86','arm')
if ($Platform -ne 'all') { $platforms=@($Platform) }
$output=Join-Path $projectRoot 'dist\fnos'
New-Item -ItemType Directory -Force $output | Out-Null
foreach ($target in $platforms) {
    $arch=if ($target -eq 'x86') {'amd64'} else {'arm64'}
    $locked=$locks | Where-Object { $_.arch -eq $arch } | Select-Object -First 1
    if (-not $locked) { throw "Missing runtime lock: $arch" }
    if (-not $locked.local) { throw 'Only locally built audio runtimes are permitted for release packaging.' }
    $archive=Join-Path $projectRoot ('dist/audio/'+$locked.archive_file)
    $runtime=Join-Path $projectRoot ('.tools/ffmpeg/'+$locked.runtime_dir)
    if (-not (Test-Path -LiteralPath $archive)) { throw 'Build audio first: python scripts/build-audio.py, then python scripts/package-audio.py' }
    if ((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne $locked.archive_sha256) { throw "Audio archive checksum mismatch: $arch" }
    New-Item -ItemType Directory -Force (Split-Path $runtime -Parent) | Out-Null
    if (-not (Test-Path -LiteralPath (Join-Path $runtime 'ffmpeg'))) {
        tar -xf $archive -C (Split-Path $runtime -Parent)
        if ($LASTEXITCODE -ne 0) { throw 'Audio extraction failed' }
    }
    if ((Get-FileHash -LiteralPath (Join-Path $runtime 'ffmpeg') -Algorithm SHA256).Hash.ToLowerInvariant() -ne $locked.binary_sha256) { throw "Audio binary checksum mismatch: $arch" }
    $sourceArchive=Join-Path $projectRoot ('dist/audio/'+$locked.source_archive)
    if ((Get-FileHash -LiteralPath $sourceArchive -Algorithm SHA256).Hash.ToLowerInvariant() -ne $locked.source_sha256) { throw 'Audio source attachment checksum mismatch' }
    Copy-Item -LiteralPath $sourceArchive -Destination $output -Force
    Copy-Item -LiteralPath ($sourceArchive+'.sha256') -Destination $output -Force
    $stage=Join-Path $projectRoot ('build/fnos/'+$target+'-'+$version+'-'+[guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force $stage | Out-Null
    Copy-Item -Path (Join-Path $projectRoot 'packaging/fnos/*') -Destination $stage -Recurse
    $stageApp=Join-Path $stage 'app'
    foreach ($directory in @('bin','audio','licenses/ffmpeg','ui/images')) { New-Item -ItemType Directory -Force (Join-Path $stageApp $directory) | Out-Null }
    Copy-Item -LiteralPath (Join-Path $projectRoot ('dist/micast-go-linux-'+$arch)) -Destination (Join-Path $stageApp 'bin/micast-go')
    Copy-Item -LiteralPath (Join-Path $runtime 'ffmpeg') -Destination (Join-Path $stageApp 'audio/ffmpeg')
    foreach ($notice in @('LICENSE.txt','LAME-COPYING','MUSL-COPYRIGHT','ZIG-LICENSE','BUILD-INFO.json','readme.txt')) { Copy-Item -LiteralPath (Join-Path $runtime $notice) -Destination (Join-Path $stageApp ('licenses/ffmpeg/'+$notice)) }
    Copy-Item -Path (Join-Path $projectRoot 'licenses/*') -Destination (Join-Path $stageApp 'licenses') -Recurse -Force
    Copy-Item -LiteralPath LICENSE -Destination (Join-Path $stageApp 'LICENSE')
    foreach ($size in @(32,48,64,96,128,192,256,512)) { Copy-Item -LiteralPath (Join-Path $projectRoot ('assets/icons/micast-go-'+$size+'.png')) -Destination (Join-Path $stageApp ('ui/images/icon_'+$size+'.png')) }
    Copy-Item -LiteralPath assets/icons/micast-go-64.png -Destination (Join-Path $stage 'ICON.PNG')
    Copy-Item -LiteralPath assets/icons/micast-go-256.png -Destination (Join-Path $stage 'ICON_256.PNG')
    $manifest=Get-Content (Join-Path $stage 'manifest') -Raw -Encoding UTF8
    [IO.File]::WriteAllText((Join-Path $stage 'manifest'),[regex]::Replace($manifest,'(?m)^platform=.+$','platform='+$target).Replace("`r`n","`n"),[Text.UTF8Encoding]::new($false))
    foreach ($script in (Get-ChildItem (Join-Path $stage 'cmd') -File)) { $text=[IO.File]::ReadAllText($script.FullName);[IO.File]::WriteAllText($script.FullName,$text.Replace("`r`n","`n"),[Text.UTF8Encoding]::new($false)) }
    Push-Location $stage
    try { $packLog=& $Fnpack build 2>&1; $packLog | ForEach-Object { Write-Host $_ }; if (($packLog -join "`n") -notmatch 'Packing successfully') { throw 'fnpack did not report success' } } finally { Pop-Location }
    $raw=Join-Path $stage 'micast-go.fpk'
    if (-not (Test-Path -LiteralPath $raw)) { throw 'fnpack output missing' }
    $artifact=Join-Path $output ('micast-go-'+$target+'-'+$version+'.fpk')
    & (Join-Path $projectRoot '.tools/fpk-tool.exe') -input $raw -output $artifact -platform $target -manifest (Join-Path $projectRoot 'packaging/fnos/manifest')
    if ($LASTEXITCODE -ne 0) { throw 'FPK structural validation failed' }
    $hash=(Get-FileHash -LiteralPath $artifact -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText(($artifact+'.sha256'),$hash+'  '+(Split-Path $artifact -Leaf)+"`n",[Text.UTF8Encoding]::new($false))
    Get-Item -LiteralPath $artifact | Select-Object Name,Length
}
