# End-to-end acceptance run: a fresh database, two real accounts, and one pass over every path a deployment
# has to survive. The API is started against the scratch schema; nothing touches the development database.
param([switch]$Keep)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
if ($root -eq '') { $root = (Get-Location).Path }
$work = Join-Path $root '.cache\acceptance'
New-Item -ItemType Directory -Path $work -Force | Out-Null
$schema = 'opentalk_acceptance'

$mysql = (docker inspect opentalk-dev-mysql-1 | ConvertFrom-Json)[0]
$binding = $mysql.NetworkSettings.Ports.'3306/tcp'[0]
$rootPassword = ($mysql.Config.Env | Where-Object { $_.StartsWith('MYSQL_ROOT_PASSWORD=') }).Substring('MYSQL_ROOT_PASSWORD='.Length)
$dsn = 'root:' + $rootPassword + '@tcp(127.0.0.1:' + $binding.HostPort + ')/' + $schema + '?parseTime=true&loc=UTC&charset=utf8mb4'

Push-Location (Join-Path $root 'server')
try {
    go build -o (Join-Path $work 'migrate.exe') ./cmd/migrate
    if ($LASTEXITCODE -ne 0) { throw 'cannot build migrate' }
    go build -o (Join-Path $work 'admin.exe') ./cmd/admin
    if ($LASTEXITCODE -ne 0) { throw 'cannot build admin' }
    go build -o (Join-Path $work 'api.exe') ./cmd/api
    if ($LASTEXITCODE -ne 0) { throw 'cannot build api' }
} finally { Pop-Location }

Write-Output '--- prepare a fresh database ---'
docker exec -e MYSQL_PWD=$rootPassword opentalk-dev-mysql-1 mysql -uroot `
    -e "DROP DATABASE IF EXISTS $schema; CREATE DATABASE $schema CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;" | Out-Null
$env:MYSQL_DSN = $dsn
$env:APP_SECRET = 'BAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='
$env:APP_ORIGIN = 'http://127.0.0.1:15173'
$env:REDIS_ADDR = '127.0.0.1:16379'
$env:TRUST_PROXY = 'false'
$env:HTTP_ADDR = '127.0.0.1:18082'
& (Join-Path $work 'migrate.exe') -action up | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'migration failed' }
# Boards are structure, not seed data for the acceptance run's own content, so they are created up front.
'unused' | & (Join-Path $work 'admin.exe') -action seed-boards 2>$null | Out-Null
Write-Output 'schema ready'

Write-Output '--- create the two accounts ---'
# create-admin is the bootstrap path that exists before anyone can sign in; login needs no captcha, so the
# acceptance run can use it for both accounts. ErrorActionPreference is relaxed for these two calls only:
# PowerShell 5.1 promotes a native command's stderr (which carries the password prompt) to a terminating error.
$previous = $ErrorActionPreference
$ErrorActionPreference = 'Continue'
'acceptadmin123' | & (Join-Path $work 'admin.exe') -action create-admin -email 'accept-admin@example.test' -nickname AcceptAdm -password-stdin
'acceptmember123' | & (Join-Path $work 'admin.exe') -action create-admin -email 'accept-member@example.test' -nickname AcceptMem -password-stdin
$ErrorActionPreference = $previous
Write-Output 'accounts ready'

$api = Start-Process -FilePath (Join-Path $work 'api.exe') -PassThru -NoNewWindow `
    -RedirectStandardOutput (Join-Path $work 'api.log') -RedirectStandardError (Join-Path $work 'api-error.log')
$exit = 1
try {
    Start-Sleep -Seconds 4
    if ($api.HasExited) { throw 'the API exited early; see .cache/acceptance/api-error.log' }
    Write-Output '--- acceptance walk ---'
    & (Join-Path $root '.cache\validation\Scripts\python.exe') (Join-Path $root 'scripts\acceptance.py') `
        'http://127.0.0.1:18082' 'http://127.0.0.1:15173' `
        'accept-admin@example.test' 'acceptadmin123' 'accept-member@example.test' 'acceptmember123'
    $exit = $LASTEXITCODE
} finally {
    if (-not $api.HasExited) { Stop-Process -Id $api.Id -Force }
    if (-not $Keep) {
        docker exec -e MYSQL_PWD=$rootPassword opentalk-dev-mysql-1 mysql -uroot -e "DROP DATABASE IF EXISTS $schema;" | Out-Null
        Write-Output 'scratch schema dropped'
    }
}
Write-Output ("ACCEPTANCE_EXIT=" + $exit)
exit $exit
