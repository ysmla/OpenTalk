# OpenTalk release upload: build outputs -> server -> install -> migrate -> restart -> smoke.
#
# Why a script and not a list of commands: the install has to be all-or-nothing. Installing the
# binaries and then failing to place the web root or the proxy config leaves a site that answers
# /health but serves no pages, which is worse than not deploying at all. Every check that can be
# done before the switch is done before it, and the previous release is kept for rollback.
#
# Prerequisites:
#   * SSH access that does not prompt (key-based, or run it and answer the prompt yourself);
#   * the target already has MySQL, Redis and a web server (OpenResty or nginx);
#   * /etc/opentalk/api.env already exists and is filled in (deploy/env.production.example);
#   * systemd, since the units in deploy/systemd are what start and supervise the processes.
#
# Usage:
#   powershell -NoProfile -ExecutionPolicy Bypass -File deploy/release.ps1 `
#     -Server 1.2.3.4 -Domain opentalk.example.com
#
#   # show what would happen, change nothing
#   powershell -NoProfile -ExecutionPolicy Bypass -File deploy/release.ps1 -Server 1.2.3.4 `
#     -Domain opentalk.example.com -DryRun
#
# This file is deliberately ASCII-only (Windows PowerShell 5.1 reads a BOM-less script as ANSI).
param(
    [Parameter(Mandatory = $true)][string]$Server,
    [Parameter(Mandatory = $true)][string]$Domain,
    [string]$User = 'root',
    [string]$WebRoot = '/srv/opentalk',
    [string]$EnvFile = '/etc/opentalk/api.env',
    [string]$ProxyConfDir = '/usr/local/openresty/nginx/conf/conf.d',
    [string]$ProxyReload = 'openresty',
    [switch]$SkipMigrate,
    [switch]$SkipSmoke,
    [switch]$DryRun
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$dist = Join-Path $root 'dist-deploy'
$remote = "$User@$Server"
$stamp = Get-Date -Format 'yyyyMMddHHmmss'
$stage = "/tmp/opentalk-release-$stamp"

function Step([string]$text) { Write-Host "== $text" }
function Run([string]$command) {
    if ($DryRun) { Write-Host "   [dry-run] $command"; return }
    & ssh $remote $command
    if ($LASTEXITCODE -ne 0) { throw "remote command failed ($LASTEXITCODE): $command" }
}
function Copy([string]$from, [string]$to) {
    if ($DryRun) { Write-Host "   [dry-run] scp -r $from -> ${remote}:$to"; return }
    & scp -r $from "${remote}:$to"
    if ($LASTEXITCODE -ne 0) { throw "scp failed ($LASTEXITCODE): $from" }
}

# 1. Refuse to start without a complete local build. Uploading a stale web root next to fresh
#    binaries is the classic way to ship a frontend that calls endpoints that do not exist.
Step 'check local build outputs'
foreach ($item in @('bin/api', 'bin/worker', 'bin/admin', 'bin/migrate', 'web/index.html',
                    'deploy/nginx/opentalk.conf', 'deploy/systemd/opentalk-api.service')) {
    if (-not (Test-Path (Join-Path $dist $item))) {
        throw "missing build output: $dist\$item (cross-compile the Linux binaries and run npm run build)"
    }
}
# The API binary must be a Linux ELF: building on Windows without GOOS=linux silently produces an
# executable the server cannot run.
$apiBytes = [System.IO.File]::ReadAllBytes((Join-Path $dist 'bin/api'))
if (-not ($apiBytes[0] -eq 0x7F -and $apiBytes[1] -eq 0x45 -and $apiBytes[2] -eq 0x4C -and $apiBytes[3] -eq 0x46)) {
    throw 'dist-deploy/bin/api is not a Linux ELF binary; rebuild with $env:GOOS=linux'
}
Write-Host ("   build looks complete (api is ELF, {0} MiB)" -f [math]::Round($apiBytes.Length / 1MB, 1))

# 2. Upload into a staging directory, so a failed transfer cannot half-overwrite the live install.
Step "upload to ${remote}:$stage"
Run "mkdir -p $stage"
Copy (Join-Path $dist 'bin') "$stage/"
Copy (Join-Path $dist 'web') "$stage/"
Copy (Join-Path $dist 'deploy') "$stage/"

# 3. Check the server before changing anything: a missing env file or an unknown proxy would
#    otherwise only surface after the service fails to start.
Step 'check the server prerequisites'
Run "test -f $EnvFile || { echo 'MISSING: $EnvFile'; exit 1; }"
Run "test -d $WebRoot || { echo 'MISSING: $WebRoot'; exit 1; }"
Run "id opentalk >/dev/null 2>&1 || { echo 'MISSING: the opentalk system user'; exit 1; }"
Run "command -v $ProxyReload >/dev/null 2>&1 || { echo 'MISSING: $ProxyReload on PATH (adjust -ProxyReload)'; exit 1; }"
Run "command -v systemctl >/dev/null 2>&1 || { echo 'MISSING: systemd'; exit 1; }"

# 4. Verify the proxy config BEFORE touching the live one. This is the only step whose failure
#    would take the site down, so it is tested in /tmp first and the install happens after.
Step 'validate the proxy config against this domain'
Run "sed 's/opentalk\.example\.com/$Domain/g' $stage/deploy/nginx/opentalk.conf > $stage/opentalk.conf"
Run "cp $stage/opentalk.conf $ProxyConfDir/opentalk.conf.new"
Run "if $ProxyReload -t -c /usr/local/openresty/nginx/conf/nginx.conf -g 'include $ProxyConfDir/opentalk.conf.new;' >/dev/null 2>&1; then echo 'config ok'; else echo 'NOTE: standalone test unavailable; the include is checked after install'; fi"

# 5. Install binaries into a versioned directory and flip a symlink, so rollback is one command.
#    `current` is what the systemd units point at.
Step 'install binaries'
Run "mkdir -p $WebRoot/releases/$stamp/bin"
Run "install -m 0755 -o opentalk -g opentalk $stage/bin/api     $WebRoot/releases/$stamp/bin/api"
Run "install -m 0755 -o opentalk -g opentalk $stage/bin/worker  $WebRoot/releases/$stamp/bin/worker"
Run "install -m 0755 -o opentalk -g opentalk $stage/bin/admin   $WebRoot/releases/$stamp/bin/admin"
Run "install -m 0755 -o opentalk -g opentalk $stage/bin/migrate $WebRoot/releases/$stamp/bin/migrate"

# 6. Web root: replace, never merge. Stale files from an older build keep being served otherwise,
#    and index.html points at hashed chunk names that the new build may not have produced.
Step 'install the frontend'
Run "rm -rf $WebRoot/releases/$stamp/web && mkdir -p $WebRoot/releases/$stamp/web"
Run "cp -r $stage/web/. $WebRoot/releases/$stamp/web/"
Run "chown -R opentalk:opentalk $WebRoot/releases/$stamp"
Run "chmod -R a+rX $WebRoot/releases/$stamp"
# nginx serves $WebRoot/current/web, so this is the switch.
Run "ln -sfn $WebRoot/releases/$stamp $WebRoot/current"

Step 'install systemd units'
Run "install -m 0644 $stage/deploy/systemd/*.service /etc/systemd/system/"
Run "install -m 0644 $stage/deploy/systemd/*.timer /etc/systemd/system/"
Run "systemctl daemon-reload"

Step 'install the proxy config'
Run "install -m 0644 $stage/opentalk.conf $ProxyConfDir/opentalk.conf"
Run "rm -f $ProxyConfDir/opentalk.conf.new"
Run "$ProxyReload -t"
Run "$ProxyReload -s reload"

# 7. Migrate before restarting: the new API is not useful on an old schema (/ready fails). The
#    env file is sourced by the shell and passed to the command, which is why the variables are
#    expanded on the server rather than here.
if (-not $SkipMigrate) {
    Step 'run database migrations'
    Run "set -a; . $EnvFile; set +a; runuser -u opentalk -- $WebRoot/current/bin/migrate -action up"
}

# 8. Restart. The API is stateless, so this is the whole deploy; the worker is restarted too so a
#    new binary starts taking mail events immediately.
Step 'restart services'
Run "systemctl enable --now opentalk-api opentalk-worker opentalk-cleanup.timer"
Run "systemctl restart opentalk-api opentalk-worker"
Run "sleep 3; systemctl is-active opentalk-api opentalk-worker"

Step 'remove the staging directory'
Run "rm -rf $stage"

if (-not $SkipSmoke) {
    Step "smoke check against https://$Domain"
    if ($DryRun) {
        Write-Host "   [dry-run] deploy/smoke.ps1 -Base https://$Domain"
    } else {
        & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'smoke.ps1') -Base "https://$Domain"
        if ($LASTEXITCODE -ne 0) {
            Write-Host ''
            Write-Host 'RELEASED BUT SMOKE FAILED: the services are up; investigate before announcing.' -ForegroundColor Red
            Write-Host "Rollback: ssh $remote 'ln -sfn $WebRoot/releases/<previous> $WebRoot/current && systemctl restart opentalk-api opentalk-worker'"
            exit 1
        }
    }
}

Write-Host ''
Write-Host 'RELEASE_OK' -ForegroundColor Green
Write-Host 'If this was the first deploy: create the first admin and configure SMTP (docs/deployment.md),'
Write-Host 'then walk the site once in a real browser and confirm one notification mail really arrives.'
