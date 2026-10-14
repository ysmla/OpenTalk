$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$apiProcess = $null
$webProcess = $null
New-Item -ItemType Directory -Path (Join-Path $projectRoot '.cache') -Force | Out-Null
$env:HTTP_ADDR = '127.0.0.1:18080'
$env:APP_ORIGIN = 'http://127.0.0.1:15173'
$env:MYSQL_DSN = 'opentalk:smoke-only@tcp(127.0.0.1:13306)/opentalk'
$env:REDIS_ADDR = '127.0.0.1:16379'
$env:DEV_API_TARGET = 'http://127.0.0.1:18080'
try {
    $apiProcess = Start-Process -FilePath (Join-Path $projectRoot 'bin/opentalk-api.exe') -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $projectRoot '.cache/api-smoke.log') -RedirectStandardError (Join-Path $projectRoot '.cache/api-smoke-error.log')
    $webProcess = Start-Process -FilePath (Get-Command node.exe).Source -ArgumentList @('node_modules/vite/bin/vite.js', '--host', '127.0.0.1', '--port', '15173') -WorkingDirectory (Join-Path $projectRoot 'web') -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $projectRoot '.cache/web-smoke.log') -RedirectStandardError (Join-Path $projectRoot '.cache/web-smoke-error.log')
    Start-Sleep -Seconds 2
    if ($apiProcess.HasExited -or $webProcess.HasExited) { throw 'A smoke process exited before checks; inspect .cache logs.' }
    $health = Invoke-WebRequest 'http://127.0.0.1:18080/health' -TimeoutSec 8
    $page = Invoke-WebRequest 'http://127.0.0.1:15173/' -TimeoutSec 8
    $proxy = Invoke-WebRequest 'http://127.0.0.1:15173/health' -TimeoutSec 8
    $ready = Invoke-WebRequest 'http://127.0.0.1:18080/ready' -SkipHttpErrorCheck -TimeoutSec 8
    $missing = Invoke-WebRequest 'http://127.0.0.1:18080/api/v1/topics' -SkipHttpErrorCheck -TimeoutSec 8
    if ($health.StatusCode -ne 200 -or $page.StatusCode -ne 200 -or $proxy.StatusCode -ne 200 -or $ready.StatusCode -ne 503 -or $missing.StatusCode -ne 404) { throw 'Unexpected smoke response status.' }
    [pscustomobject]@{ api_health = $health.StatusCode; frontend = $page.StatusCode; vite_proxy = $proxy.StatusCode; unavailable_dependencies = $ready.StatusCode; unimplemented_route = $missing.StatusCode } | ConvertTo-Json
} finally {
    # Only terminate the two child processes started by this script.
    if ($null -ne $webProcess -and -not $webProcess.HasExited) { Stop-Process -Id $webProcess.Id }
    if ($null -ne $apiProcess -and -not $apiProcess.HasExited) { Stop-Process -Id $apiProcess.Id }
}
