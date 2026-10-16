# Launches only the development binary with an isolated temporary account store.
param([switch]$FullRuntime)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path $PSScriptRoot -Parent
$binaryPath = Join-Path $projectRoot 'dist\micast-go-windows-amd64.exe'
if (-not (Test-Path -LiteralPath $binaryPath)) { throw 'Run scripts/build.ps1 first.' }
$smokeRoot = Join-Path $projectRoot ('.run\smoke-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force $smokeRoot | Out-Null
$probeSocket = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
$probeSocket.Start()
$smokePort = $probeSocket.LocalEndpoint.Port
$probeSocket.Stop()
$baseUrl = 'http://127.0.0.1:' + $smokePort
$process = $null
try {
	$password='micast-go-smoke-password'
	$setupInfo=[System.Diagnostics.ProcessStartInfo]::new($binaryPath)
	$setupInfo.UseShellExecute=$false
	$setupInfo.CreateNoWindow=$true
	$setupInfo.RedirectStandardInput=$true
	$setupInfo.ArgumentList.Add('--data-dir')
	$setupInfo.ArgumentList.Add($smokeRoot)
	$setupInfo.ArgumentList.Add('--set-admin-password')
	$setupProcess=[System.Diagnostics.Process]::Start($setupInfo)
	$setupProcess.StandardInput.Write($password)
	$setupProcess.StandardInput.Close()
	$setupProcess.WaitForExit()
	if ($setupProcess.ExitCode -ne 0) { throw 'Cannot set test management password.' }
    $adminKey = (& $binaryPath --data-dir $smokeRoot --show-access-key).Trim()
    $headers = @{ Authorization = 'Bearer ' + $adminKey }
    foreach ($pass in @(1,2)) {
        $arguments = @('--listen', ('127.0.0.1:' + $smokePort), '--data-dir', ('"' + $smokeRoot + '"'), '--no-receivers')
		if ($FullRuntime) { $arguments=@('--listen', ('127.0.0.1:' + $smokePort), '--data-dir', ('"' + $smokeRoot + '"'), '--no-discovery', '--airplay-listen', '127.0.0.1:0') }
        $process = Start-Process -FilePath $binaryPath -ArgumentList $arguments -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $smokeRoot "stdout-$pass.log") -RedirectStandardError (Join-Path $smokeRoot "stderr-$pass.log")
        $ready = $false
        for ($attempt = 0; $attempt -lt 50; $attempt++) {
            try { $health = Invoke-RestMethod ($baseUrl + '/healthz') -TimeoutSec 1; if ($health.status -eq 'ok') { $ready = $true; break } } catch { Start-Sleep -Milliseconds 100 }
        }
        if (-not $ready) { throw 'Service failed to become ready.' }
        $unauthorized = Invoke-WebRequest ($baseUrl + '/api/status') -SkipHttpErrorCheck
        if ($unauthorized.StatusCode -ne 401) { throw 'Unauthenticated status was accessible.' }
        $status = Invoke-RestMethod ($baseUrl + '/api/status') -Headers $headers
        if ($status.version -ne (& $binaryPath --version).Trim()) { throw 'Service version metadata mismatch.' }
        if ($status.account.status -ne 'disconnected') { throw 'Unexpected account state.' }
		if ($FullRuntime -and ($status.receivers.audio -ne 'ready')) { throw 'Protocol runtime did not start.' }
		Invoke-RestMethod ($baseUrl + '/app/micast-go/api/access') -Method Post -Headers @{Origin=$baseUrl; 'Sec-Fetch-Site'='same-origin'; 'X-MiCast-Request'='1'; 'X-MiCast-CSRF'=(Invoke-RestMethod ($baseUrl + '/api/access')).csrf} -ContentType 'application/json' -Body ('{"key":"'+$password+'"}') -SessionVariable managementSession | Out-Null
		$cookieStatus=Invoke-RestMethod ($baseUrl + '/app/micast-go/api/status') -WebSession $managementSession
		if ($cookieStatus.account.status -ne 'disconnected') { throw 'Password cookie did not authenticate gateway API.' }
        if ($FullRuntime) {
            $routes=Invoke-RestMethod ($baseUrl + '/api/routes') -Headers $headers
            if ($routes.Count -ne 0) { throw 'An entry was published without any speaker targets.' }

        }

        if ($pass -eq 1) {
            Invoke-RestMethod ($baseUrl + '/api/settings') -Method Put -Headers $headers -ContentType 'application/json' -Body '{"mode":"whole_house","selected":"","name":"MiCast Go Smoke","format":"mp3"}' | Out-Null
        } elseif ($status.config.name -ne 'MiCast Go Smoke') { throw 'Configuration did not survive restart.' }
        foreach ($resource in @('/app/micast-go/support.js','/app/micast-go/user-agreement.txt','/app/micast-go/privacy-policy.txt','/app/micast-go/wechat-appreciation.jpg','/app/micast-go/alipay-support.jpg','/lucide.svg','/app/micast-go/lucide.svg','/qr.js','/app/micast-go/qr.js','/','/app.js','/style.css','/icon.svg','/app/micast-go/','/app/micast-go/app.js','/app/micast-go/style.css','/app/micast-go/icon.svg')) {
            $response = Invoke-WebRequest ($baseUrl + $resource)
            if ($response.StatusCode -ne 200 -or $response.RawContentLength -eq 0) { throw "Missing UI resource: $resource" }
        }
        Stop-Process -Id $process.Id -Force
        $process.WaitForExit()
        $process = $null
    }
    [pscustomobject]@{ Result='passed'; Starts=2; Authentication='passed'; ManagementPassword='passed'; GatewayPaths='passed'; FullRuntime=[bool]$FullRuntime; EmbeddedUI='passed'; ConfigurationRestart='passed'; Data=$smokeRoot } | ConvertTo-Json
} finally {
    if ($null -ne $process -and -not $process.HasExited) { Stop-Process -Id $process.Id -Force }
}
