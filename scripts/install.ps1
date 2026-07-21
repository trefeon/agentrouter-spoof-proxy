# AgentRouter Spoof Proxy — Windows One-Click Installer
#
# Run in PowerShell (as Administrator):
#   iwr -useb https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.ps1 | iex
#
# Or right-click → Run with PowerShell

param()

$ErrorActionPreference = "Stop"
$repo = "https://github.com/trefeon/agentrouter-spoof-proxy.git"
$installDir = "$env:USERPROFILE\agentrouter-spoof-proxy"

function Write-Info  { Write-Host "[INFO]  $args" -ForegroundColor Cyan }
function Write-OK    { Write-Host "[OK]    $args" -ForegroundColor Green }
function Write-Warn  { Write-Host "[WARN]  $args" -ForegroundColor Yellow }
function Write-Error { Write-Host "[ERROR] $args" -ForegroundColor Red }

Write-Info "AgentRouter Spoof Proxy — Windows Installer"
Write-Info "--------------------------------------------"
Write-Host ""

# ── Check Node.js ──
try {
    $nodeVer = (node -v) -replace 'v',''
    Write-OK "Node.js $nodeVer found"
    if ([int]($nodeVer.Split('.')[0]) -lt 22) {
        Write-Warn "Node.js $nodeVer — recommended 22+. Continuing anyway..."
    }
} catch {
    Write-Error "Node.js not found!"
    Write-Host "Install from: https://nodejs.org (LTS, 22+)"
    Write-Host "Or via winget: winget install OpenJS.NodeJS.LTS"
    pause; exit 1
}

# ── Check Docker (optional) ──
$hasDocker = $false
try {
    docker --version | Out-Null
    docker info | Out-Null
    $hasDocker = $true
} catch {}

# ── Clone repo ──
if (Test-Path $installDir) {
    Write-Info "Directory exists, pulling latest..."
    Set-Location $installDir
    git pull
} else {
    Write-Info "Cloning repository..."
    git clone $repo $installDir
    Set-Location $installDir
}

# ── Setup .env ──
if (-not (Test-Path ".env")) {
    Copy-Item .env.example .env
    Write-OK ".env created (using defaults)"
}

# ── Pick install method ──
Write-Host ""
Write-Host "How do you want to run the proxy?"
Write-Host "  1) Docker Desktop  (recommended — isolated)"
Write-Host "  2) PM2             (runs on host, auto-restart)"
Write-Host "  3) Direct node     (foreground, for testing)"
Write-Host ""
$choice = Read-Host "Choice [1]"
if ($choice -eq "") { $choice = "1" }

switch ($choice) {
    "1" {
        if (-not $hasDocker) { Write-Error "Docker not available. Choose 2 or 3."; pause; exit 1 }
        Write-Info "Starting with Docker Compose..."
        docker compose up -d --build
        Start-Sleep -Seconds 5
        Write-Info "Waiting for WAF warmup..."
        Start-Sleep -Seconds 3
        try {
            $health = iwr -useb http://localhost:8318/health | ConvertFrom-Json
            Write-OK "Proxy running! http://localhost:8318"
            Write-Info "wafCookie=$($health.wafCookie) models=$($health.availableModels)"
        } catch {
            Write-Warn "Proxy started but health check failed. Run: docker logs agentrouter-proxy"
        }
    }
    "2" {
        Write-Info "Installing PM2..."
        npm install -g pm2
        pm2 start proxy.mjs --name agentrouter-proxy
        pm2 save
        Write-Info "Auto-start: pm2 startup (follow printed instructions, run as Admin)"
        Start-Sleep -Seconds 3
        try {
            iwr -useb http://localhost:8318/health | Out-Null
            Write-OK "Proxy running! http://localhost:8318"
        } catch {
            Write-Warn "WAF still warming up. Wait a few seconds."
        }
        Write-Host ""
        Write-Info "Manage: pm2 status | pm2 logs agentrouter-proxy | pm2 restart agentrouter-proxy"
    }
    "3" {
        Write-Info "Starting proxy in foreground (Ctrl+C to stop)..."
        Write-Host ""
        node proxy.mjs
    }
    default {
        Write-Error "Invalid choice."; pause; exit 1
    }
}

Write-Host ""
Write-OK "Done! Proxy is at http://localhost:8318"
Write-Host ""
Write-Info "Next steps:"
Write-Host "  - 9Router: Add custom OpenAI Compatible provider → Base URL: http://localhost:8318/v1 → Import from /models"
Write-Host "  - Direct:  curl http://localhost:8318/v1/messages -H 'Authorization: Bearer YOUR_KEY' -d '{\"model\":\"claude-opus-4-8\",\"max_tokens\":10,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
Write-Host "  - Docs:   https://github.com/trefeon/agentrouter-spoof-proxy/blob/main/docs/panduan-9router.md"
Write-Host ""
pause
