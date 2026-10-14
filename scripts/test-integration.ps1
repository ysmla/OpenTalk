$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$testRedisID = $null
Push-Location (Join-Path $projectRoot 'server')
try {
    # Credentials come from the already-running development container and are never printed, so no password
    # is stored in this script and the check cannot rot into a documented credential that no longer matches.
    $mysql = (docker inspect opentalk-dev-mysql-1 | ConvertFrom-Json)[0]
    if ($LASTEXITCODE -ne 0 -or -not $mysql.State.Running) { throw 'The local development MySQL container is not running.' }
    $binding = $mysql.NetworkSettings.Ports.'3306/tcp'[0]
    if ($binding.HostIp -ne '127.0.0.1') { throw 'Expected a loopback-only development database.' }
    $entry = $mysql.Config.Env | Where-Object { $_.StartsWith('MYSQL_ROOT_PASSWORD=') }
    if (-not $entry) { throw 'Local MySQL test credential unavailable.' }
    $password = $entry.Substring('MYSQL_ROOT_PASSWORD='.Length)
    $env:OPENTALK_TEST_MYSQL_DSN = 'root:' + $password + '@tcp(127.0.0.1:' + $binding.HostPort + ')/?parseTime=true'
    # An isolated Redis, deleted afterwards, so the running development Redis (used on database 15 by the
    # account tests) is never touched.
    $testRedisID = docker run --rm -d --name ('opentalk-review-redis-' + [guid]::NewGuid().ToString('N').Substring(0, 8)) -p '127.0.0.1::6379' redis:8.2-alpine
    if ($LASTEXITCODE -ne 0) { throw 'Cannot start isolated Redis.' }
    $redisBinding = (docker inspect $testRedisID | ConvertFrom-Json)[0].NetworkSettings.Ports.'6379/tcp'[0]
    $env:OPENTALK_TEST_REDIS_ADDR = '127.0.0.1:' + $redisBinding.HostPort
    New-Item -ItemType Directory -Path ../.cache -Force | Out-Null
    # Quote flags containing relative paths: PowerShell can otherwise split these arguments.
    #
    # There is no longer a separate privacy-probe build tag: the three review findings (pending images,
    # hidden ancestors, metadata stripping) are ordinary tests in ./internal/content and ./internal/media
    # now, so one full run covers them.
    go test -count=1 -timeout 20m -json '-coverpkg=./...' '-coverprofile=../.cache/review-coverage.out' ./... | Out-File ../.cache/review-go-tests.jsonl -Encoding utf8
    $result = $LASTEXITCODE
    Write-Output ('TEST_EXIT=' + $result)
    exit $result
} finally {
    if ($testRedisID) { docker stop $testRedisID | Out-Null }
    Remove-Item Env:OPENTALK_TEST_MYSQL_DSN -ErrorAction SilentlyContinue
    Remove-Item Env:OPENTALK_TEST_REDIS_ADDR -ErrorAction SilentlyContinue
    Pop-Location
}
