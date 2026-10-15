$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$apiProcess = $null
$webProcess = $null
New-Item -ItemType Directory -Path (Join-Path $projectRoot '.cache') -Force | Out-Null
$apiBinary = Join-Path $projectRoot '.cache/smoke-api.exe'
Push-Location (Join-Path $projectRoot 'server')
try {
    go build -o $apiBinary ./cmd/api
    if ($LASTEXITCODE -ne 0) { throw 'Cannot build current API for smoke checks.' }
} finally { Pop-Location }
# An isolated, ephemeral key; never reuse the site's real encryption key in this test.
# RNGCryptoServiceProvider is used instead of RandomNumberGenerator::Fill because this script has to run on
# Windows PowerShell 5.1 as well, where that static helper does not exist.
$secretBytes = [byte[]]::new(32)
$rng = [System.Security.Cryptography.RNGCryptoServiceProvider]::new()
try { $rng.GetBytes($secretBytes) } finally { $rng.Dispose() }
$env:APP_SECRET = [Convert]::ToBase64String($secretBytes)
$env:TRUST_PROXY = 'false'
$env:HTTP_ADDR = '127.0.0.1:18080'
$env:APP_ORIGIN = 'http://127.0.0.1:15173'
$env:DEV_API_TARGET = 'http://127.0.0.1:18080'

# The database credential is read from the already-running development container instead of being written
# into this script, so no password lives in the repository and the smoke check cannot rot into a documented
# credential that no longer matches. It is used to build a DSN and is never printed.
$mysql = (docker inspect opentalk-dev-mysql-1 | ConvertFrom-Json)[0]
if ($LASTEXITCODE -ne 0 -or -not $mysql.State.Running) { throw 'The local development MySQL container is not running.' }
$mysqlBinding = $mysql.NetworkSettings.Ports.'3306/tcp'[0]
if ($mysqlBinding.HostIp -ne '127.0.0.1') { throw 'Expected a loopback-only development database.' }
$entry = $mysql.Config.Env | Where-Object { $_.StartsWith('MYSQL_ROOT_PASSWORD=') }
if (-not $entry) { throw 'Local MySQL credential unavailable.' }
$env:MYSQL_DSN = 'root:' + $entry.Substring('MYSQL_ROOT_PASSWORD='.Length) + '@tcp(127.0.0.1:' + $mysqlBinding.HostPort + ')/opentalk?parseTime=true'
$env:REDIS_ADDR = '127.0.0.1:16379'

# getStatus returns the HTTP status of a request whose non-2xx answer is the expected result. It is
# written with a catch instead of -SkipHttpErrorCheck so the script behaves identically on Windows
# PowerShell 5.1, which this project's machine actually has, and on PowerShell 7.
function Get-SmokeStatus {
    param([string]$Uri)
    try {
        return (Invoke-WebRequest $Uri -TimeoutSec 8 -UseBasicParsing).StatusCode
    } catch {
        $response = $_.Exception.Response
        if ($null -eq $response) { throw }
        return [int]$response.StatusCode
    }
}

try {
    $apiProcess = Start-Process -FilePath $apiBinary -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $projectRoot '.cache/api-smoke.log') -RedirectStandardError (Join-Path $projectRoot '.cache/api-smoke-error.log')
    $webProcess = Start-Process -FilePath (Get-Command node.exe).Source -ArgumentList @('node_modules/vite/bin/vite.js', '--host', '127.0.0.1', '--port', '15173') -WorkingDirectory (Join-Path $projectRoot 'web') -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $projectRoot '.cache/web-smoke.log') -RedirectStandardError (Join-Path $projectRoot '.cache/web-smoke-error.log')
    Start-Sleep -Seconds 3
    if ($apiProcess.HasExited) { throw 'The API process exited before the checks; inspect .cache/api-smoke*.log.' }
    if ($webProcess.HasExited) { throw 'The Vite process exited before the checks; inspect .cache/web-smoke*.log.' }
    $health = Invoke-WebRequest 'http://127.0.0.1:18080/health' -TimeoutSec 8 -UseBasicParsing
    $page = Invoke-WebRequest 'http://127.0.0.1:15173/' -TimeoutSec 8 -UseBasicParsing
    $proxy = Invoke-WebRequest 'http://127.0.0.1:15173/health' -TimeoutSec 8 -UseBasicParsing
    $ready = Get-SmokeStatus 'http://127.0.0.1:18080/ready'
    # A route the contract still marks planned, so the probe keeps proving that nothing is mounted there.
    # It has moved twice as planned features landed: first GET /api/v1/topics, then GET /api/v1/search, and
    # now the personal content list.
    $missing = Get-SmokeStatus 'http://127.0.0.1:18080/api/v1/me/contents'
    # A media key that does not exist must not be distinguishable from one that may not be shown.
    $unknownMedia = Get-SmokeStatus 'http://127.0.0.1:18080/media/00000000000000000000000000000000'
    # MySQL, Redis and the schema version are all real here, so readiness must actually succeed rather
    # than being asserted as unavailable.
    if ($health.StatusCode -ne 200 -or $page.StatusCode -ne 200 -or $proxy.StatusCode -ne 200 -or $ready -ne 200 -or $missing -ne 404 -or $unknownMedia -ne 404) { throw 'Unexpected smoke response status.' }
    [pscustomobject]@{ api_health = $health.StatusCode; frontend = $page.StatusCode; vite_proxy = $proxy.StatusCode; ready = $ready; unimplemented_route = $missing; unknown_media = $unknownMedia } | ConvertTo-Json
} finally {
    # Only terminate the two child processes started by this script.
    if ($null -ne $webProcess -and -not $webProcess.HasExited) { Stop-Process -Id $webProcess.Id }
    if ($null -ne $apiProcess -and -not $apiProcess.HasExited) { Stop-Process -Id $apiProcess.Id }
}
