# OpenTalk production smoke check (read-only, plus one optional login write path).
#
# Purpose: after deploying, prove that "reverse proxy -> API -> database" works on the
# REAL domain, not just on 127.0.0.1. It is not a substitute for a browser walkthrough,
# and it never sends mail.
#
# Usage (run this from the machine where DNS reaches your site):
#   powershell -NoProfile -ExecutionPolicy Bypass -File deploy/smoke.ps1 -Base https://opentalk.example.com
#   powershell -NoProfile -ExecutionPolicy Bypass -File deploy/smoke.ps1 -Base https://opentalk.example.com `
#       -AdminEmail you@example.com -AdminPassword '...'
#
# With an admin account it also performs one real login: a successful login proves the
# exact-Origin check, the session cookie and the CSRF derivation are all working.
# That password appears on the command line, so prefer running only the anonymous part.
#
# This file is deliberately ASCII-only: Windows PowerShell 5.1 reads a BOM-less script as
# ANSI, and non-ASCII comments would corrupt the parse before a single check runs.
param(
    [Parameter(Mandatory = $true)][string]$Base,
    [string]$AdminEmail,
    [string]$AdminPassword
)
$ErrorActionPreference = 'Stop'
$Base = $Base.TrimEnd('/')
$script:failed = 0

function Check([string]$name, [bool]$ok, [string]$detail = '') {
    if ($ok) {
        Write-Host ("PASS {0}{1}" -f $name, $(if ($detail) { ": $detail" } else { '' }))
    } else {
        $script:failed++
        Write-Host ("FAIL {0}{1}" -f $name, $(if ($detail) { ": $detail" } else { '' }))
    }
}

function Get-Status([string]$path) {
    try {
        $response = Invoke-WebRequest -Uri ($Base + $path) -UseBasicParsing -TimeoutSec 15
        return [int]$response.StatusCode
    } catch {
        if ($_.Exception.Response) { return [int]$_.Exception.Response.StatusCode }
        return 0
    }
}

# Fetch a page, returning the body on 200 and '' on anything else. Used for the HTML checks,
# where "no page" must be reported as a failed check rather than terminating the script.
function Get-Html([string]$path) {
    try {
        $response = Invoke-WebRequest -Uri ($Base + $path) -UseBasicParsing -TimeoutSec 15
        if ([int]$response.StatusCode -ne 200) { return '' }
        return [string]$response.Content
    } catch {
        return ''
    }
}

Write-Host "=== $Base ==="

# 1. Liveness and readiness. /ready also checks MySQL, Redis and the schema version, so it
#    is the endpoint that answers "can this instance serve traffic yet".
Check '/health returns 200' ((Get-Status '/health') -eq 200)
Check '/ready returns 200 (MySQL/Redis/schema ready)' ((Get-Status '/ready') -eq 200)

# 2. Static frontend and SPA fallback. The app is hash-routed, so every path must still
#    return index.html; otherwise a refresh on a sub-path breaks.
#    NOTE: the Go API does NOT serve the frontend. If this fails while /health passed, the
#    proxy is not serving web/dist (see deploy/nginx/opentalk.conf, root + try_files).
$indexHtml = Get-Html '/'
Check 'home serves the built frontend' ($indexHtml -match '<div id="root">')
Check 'SPA fallback: unknown path still serves index.html' ((Get-Html '/this-path-does-not-exist') -match '<div id="root">')

# 3. Same-origin forwarding: these three prefixes must reach the API, not the static files.
$topics = Get-Html '/api/v1/topics'
Check '/api/v1/topics returns a JSON list' ($topics -match '"items"')
Check '/media with an unknown key returns 404' ((Get-Status '/media/does-not-exist') -eq 404)

# 4. Public reads need no session.
foreach ($path in @('/api/v1/boards', '/api/v1/tags', '/api/v1/search?q=test')) {
    Check "$path readable anonymously" ((Get-Status $path) -eq 200)
}

# 5. Session endpoints answer 401 (not 404/500) for a guest.
Check 'GET /api/v1/me is 401 for a guest' ((Get-Status '/api/v1/me') -eq 401)
Check 'GET /api/v1/auth/csrf is 401 for a guest' ((Get-Status '/api/v1/auth/csrf') -eq 401)

# 6. Optional real login.
if ($AdminEmail -and $AdminPassword) {
    $session = New-Object Microsoft.PowerShell.Commands.WebRequestSession
    $headers = @{ Origin = $Base }
    try {
        $login = Invoke-RestMethod -Uri ($Base + '/api/v1/auth/login') -Method Post `
            -WebSession $session -Headers $headers -ContentType 'application/json' `
            -Body (@{ email = $AdminEmail; password = $AdminPassword } | ConvertTo-Json)
        Check 'admin login returns a csrf_token' ($null -ne $login.csrf_token)
        $me = Invoke-RestMethod -Uri ($Base + '/api/v1/me') -WebSession $session
        Check 'GET /me returns the same account' ($me.email -eq $AdminEmail) $me.email
        Check 'account role is admin' ($me.role -eq 'admin') $me.role
    } catch {
        Check 'admin login succeeds' $false $_.Exception.Message
    }
}

Write-Host ''
if ($script:failed -gt 0) {
    Write-Host "SMOKE_FAILED=$script:failed"
    exit 1
}
Write-Host 'SMOKE_OK'
