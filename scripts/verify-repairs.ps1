# Isolated review environment for the repair verification run.
#
# Creates a uniquely named scratch schema, two disposable accounts (one ordinary
# user, one administrator), a temporary API and a temporary Vite, runs the
# browser verification against them, and then removes everything it created.
# It never touches the development database or the development accounts.
param(
    [int]$ApiPort = 18084,
    [int]$WebPort = 15174,
    [switch]$Keep,
    # Diagnostic only: run one ad-hoc probe script instead of the verification run.
    [string]$Probe = ''
)
$env:OPENTALK_HTTP_DEBUG = 'off'
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
if ($root -eq '') { $root = (Get-Location).Path }
$work = Join-Path $root '.cache\repair-verification'
New-Item -ItemType Directory -Path $work -Force | Out-Null

# The schema name is unique per run so two runs cannot collide and a failed run
# cannot be mistaken for a clean one.
$stamp = Get-Date -Format 'yyyyMMddHHmmss'
$schema = 'opentalk_repair_' + $stamp
$container = 'opentalk-dev-mysql-1'
$rootPassword = (docker inspect $container | ConvertFrom-Json)[0].Config.Env |
    Where-Object { $_.StartsWith('MYSQL_ROOT_PASSWORD=') } |
    ForEach-Object { $_.Substring('MYSQL_ROOT_PASSWORD='.Length) }
$port = (docker inspect $container | ConvertFrom-Json)[0].NetworkSettings.Ports.'3306/tcp'[0].HostPort

$env:MYSQL_DSN = "root:$rootPassword@tcp(127.0.0.1:$port)/$schema" + '?parseTime=true&loc=UTC&charset=utf8mb4'
$env:REDIS_ADDR = '127.0.0.1:16379'
$env:APP_ORIGIN = "http://127.0.0.1:$WebPort"
$env:HTTP_ADDR = "127.0.0.1:$ApiPort"
$env:TRUST_PROXY = 'false'
# A throwaway secret, exactly 32 bytes after decoding: this environment is deleted at the end of the run.
$env:APP_SECRET = 'VkVSSUZZLVJFUEFJUi1TQ1JBVENILVNFQ1JFVC0zMkI='
$env:DEV_API_TARGET = "http://127.0.0.1:$ApiPort"

$memberPassword = 'verifymember1'
$adminPassword = 'verifyadmin1'

Write-Output "--- build ---"
Push-Location (Join-Path $root 'server')
try {
    go build -o (Join-Path $work 'migrate.exe') ./cmd/migrate
    if ($LASTEXITCODE -ne 0) { throw 'cannot build migrate' }
    go build -o (Join-Path $work 'admin.exe') ./cmd/admin
    if ($LASTEXITCODE -ne 0) { throw 'cannot build admin' }
    go build -o (Join-Path $work 'api.exe') ./cmd/api
    if ($LASTEXITCODE -ne 0) { throw 'cannot build api' }
} finally { Pop-Location }

Write-Output "--- scratch schema $schema ---"
docker exec -e MYSQL_PWD=$rootPassword $container mysql -uroot `
    -e "DROP DATABASE IF EXISTS $schema; CREATE DATABASE $schema CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;" | Out-Null
& (Join-Path $work 'migrate.exe') -action up | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'migration failed' }
'unused' | & (Join-Path $work 'admin.exe') -action seed-boards 2>$null | Out-Null

Write-Output '--- disposable accounts ---'
# Registration needs a real mailbox and a captcha, so both accounts come from the
# bootstrap command. Its password comes from stdin, so each call gets its own
# here-string: piping two different passwords through one pipeline delivered the
# first value to both processes, which silently made the "member" the same account
# as the administrator and invalidated every ordinary-user assertion below.
$previous = $ErrorActionPreference
$ErrorActionPreference = 'Continue'
$adminPassword | & (Join-Path $work 'admin.exe') -action create-admin -email 'verify-admin@example.test' -nickname VerifyAdm -password-stdin
$memberPassword | & (Join-Path $work 'admin.exe') -action create-admin -email 'verify-member@example.test' -nickname VerifyMem -password-stdin
$ErrorActionPreference = $previous

# create-admin can only mint administrators, and the verification run asserts what
# an ordinary user may do, so the member is demoted — and both the role and the
# password are then proved through the real API before anything else runs.
docker exec -e MYSQL_PWD=$rootPassword $container mysql -uroot `
    -e "UPDATE $schema.users SET role='user' WHERE email_normalized='verify-member@example.test';" | Out-Null
$role = (docker exec -e MYSQL_PWD=$rootPassword $container mysql -uroot -N -B `
    -e "SELECT role FROM $schema.users WHERE email_normalized='verify-member@example.test';").Trim()
if ($role -ne 'user') { throw "the member account is not an ordinary user: '$role'" }
$accountCount = (docker exec -e MYSQL_PWD=$rootPassword $container mysql -uroot -N -B `
    -e "SELECT COUNT(DISTINCT email_normalized) FROM $schema.users;").Trim()
if ($accountCount -ne '2') { throw "expected 2 accounts, found $accountCount" }

$api = $null
$web = $null
$exit = 1
try {
    $api = Start-Process -FilePath (Join-Path $work 'api.exe') -PassThru -NoNewWindow `
        -RedirectStandardOutput (Join-Path $work 'api.log') -RedirectStandardError (Join-Path $work 'api-error.log')
    Start-Sleep -Seconds 4
    if ($api.HasExited) { throw 'the API exited early; see .cache/repair-verification/api-error.log' }

    Write-Output '--- vite ---'
    $web = Start-Process -FilePath 'npm.cmd' -ArgumentList @('run', 'dev', '--', '--port', "$WebPort") `
        -WorkingDirectory (Join-Path $root 'web') -PassThru -NoNewWindow `
        -RedirectStandardOutput (Join-Path $work 'web.log') -RedirectStandardError (Join-Path $work 'web-error.log')
    $ready = $false
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        Start-Sleep -Seconds 1
        try {
            $response = Invoke-WebRequest -Uri "http://127.0.0.1:$WebPort/" -UseBasicParsing -TimeoutSec 3
            if ($response.StatusCode -eq 200) { $ready = $true; break }
        } catch { }
    }
    if (-not $ready) { throw "vite did not answer on 127.0.0.1:$WebPort; see .cache/repair-verification/web-error.log" }

    Write-Output '--- browser verification ---'
    $target = if ($Probe -ne '') { $Probe } else { Join-Path $root 'scripts\verify-fixes-browser.py' }
    & (Join-Path $root '.cache\validation\Scripts\python.exe') $target "http://127.0.0.1:$WebPort"
    $exit = $LASTEXITCODE
    Write-Output '--- db ground truth ---'
    docker exec -e MYSQL_PWD=$rootPassword $container mysql -uroot -t `
        -e "SELECT u.id, u.email_normalized, u.role, p.email_on_comment, p.email_on_answer, p.email_on_reply FROM $schema.users u LEFT JOIN $schema.notification_preferences p ON p.user_id=u.id; SELECT user_id, topic_id, email_override FROM $schema.topic_subscriptions; SELECT id,user_id,token_hash IS NOT NULL AS has_hash FROM $schema.sessions;"
} finally {
    if ($web -ne $null -and -not $web.HasExited) { Stop-Process -Id $web.Id -Force -ErrorAction SilentlyContinue }
    if ($api -ne $null -and -not $api.HasExited) { Stop-Process -Id $api.Id -Force -ErrorAction SilentlyContinue }
    # Vite spawns a child process; make sure the port is actually free again.
    Get-NetTCPConnection -LocalPort $WebPort -State Listen -ErrorAction SilentlyContinue |
        ForEach-Object { Stop-Process -Id $_.OwningProcess -Force -ErrorAction SilentlyContinue }
    if (-not $Keep) {
        docker exec -e MYSQL_PWD=$rootPassword $container mysql -uroot -e "DROP DATABASE IF EXISTS $schema;" | Out-Null
        Write-Output "scratch schema $schema dropped"
    } else {
        Write-Output "kept scratch schema $schema"
    }
}
Write-Output ("REPAIR_VERIFICATION_EXIT=" + $exit)
exit $exit
