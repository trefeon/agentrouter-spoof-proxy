# AgentRouter Spoof Proxy — Windows One-Click Installer
#
# Usage:
#   iwr -useb https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.ps1 | iex
#   iwr -useb https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.ps1 | iex "args"
#
# Parameters:
#   -Docker         Run with Docker Compose
#   -PM2            Run with PM2 on host
#   -Direct         Run with node in foreground
#   -Yes            Non-interactive mode
#   -DryRun         Print what would happen without changing system
#   -InstallDir     Install directory (default: ~\agentrouter-spoof-proxy)

param(
  [switch]$Docker,
  [switch]$PM2,
  [switch]$Direct,
  [switch]$Yes,
  [switch]$DryRun,
  [string]$InstallDir = "$env:USERPROFILE\agentrouter-spoof-proxy"
)

$ErrorActionPreference = "Stop"
$repo = "https://github.com/trefeon/agentrouter-spoof-proxy.git"
$tarballUrl = "https://github.com/trefeon/agentrouter-spoof-proxy/archive/refs/heads/main.tar.gz"
$method = ""

function Write-Info  { Write-Host "[INFO]  $args" -ForegroundColor Cyan }
function Write-OK    { Write-Host "[OK]    $args" -ForegroundColor Green }
function Write-Warn  { Write-Host "[WARN]  $args" -ForegroundColor Yellow }
function Write-Error { Write-Host "[ERROR] $args" -ForegroundColor Red }

function Run-Step {
  param([string]$Desc, [scriptblock]$Block)
  if ($DryRun) {
    Write-Info "DRY-RUN: $Desc"
    return
  }
  & $Block
}

function Confirm-Step {
  param([string]$Prompt)
  if ($Yes) { return $true }
  if (-not ([Console]::IsInputRedirected)) {
    $answer = Read-Host "$Prompt [y/N]"
    return $answer -match '^(y|yes)$'
  }
  return $false
}

Write-Info "AgentRouter Spoof Proxy — Windows Installer"
Write-Info "--------------------------------------------"
Write-Host ""
if ($DryRun) { Write-Warn "Dry-run mode enabled; no changes will be made." }

# ── Detect Node.js ──
$nodeOk = $false
try {
  $nodeVer = (node -v) -replace 'v',''
  $verParts = $nodeVer -split '\.'
  if ([int]$verParts[0] -lt 22) {
    Write-Warn "Node.js $nodeVer found. Recommended 22+. Continuing anyway."
  } else {
    Write-OK "Node.js $nodeVer"
  }
  $nodeOk = $true
} catch {
  Write-Warn "Node.js not found."
  if ($Yes) {
    Write-Info "Attempting install via winget..."
    Run-Step "winget install OpenJS.NodeJS.LTS" { winget install OpenJS.NodeJS.LTS -e --accept-source-agreements }
    try { node --version | Out-Null; $nodeOk = $true } catch {}
  } else {
    Write-Host "  Install: https://nodejs.org (LTS, 22+)"
    Write-Host "  Or: winget install OpenJS.NodeJS.LTS"
  }
}
if (-not $nodeOk -and -not $DryRun) {
  Write-Error "Node.js is required."
  pause; exit 1
}

# ── Detect Docker (optional) ──
$hasDocker = $false
try {
  docker --version | Out-Null
  docker info | Out-Null
  $hasDocker = $true
} catch {}

# ── Clone or download repo ──
if (Test-Path (Join-Path $InstallDir "proxy.mjs")) {
  Write-Info "Found project at $InstallDir"
  Set-Location $InstallDir
} elseif (Test-Path "proxy.mjs") {
  $InstallDir = (Get-Location).Path
} else {
  Write-Info "Installing to $InstallDir"

  if (-not (Test-Path $InstallDir)) {
    try {
      git --version | Out-Null
      $hasGit = $true
    } catch { $hasGit = $false }

    if ($hasGit) {
      Run-Step "git clone $repo $InstallDir" { git clone $repo $InstallDir }
    } else {
      Write-Warn "Git not found. Downloading tarball instead..."
      Run-Step "Download and extract tarball to $InstallDir" {
        $tmp = "$env:TEMP\agentrouter-install"
        New-Item -ItemType Directory -Force -Path $tmp | Out-Null
        $tarballPath = "$tmp\source.tar.gz"
        Invoke-WebRequest -Uri $tarballUrl -OutFile $tarballPath
        tar -xzf $tarballPath -C $tmp
        if (Test-Path $InstallDir) { Remove-Item -Recurse -Force $InstallDir }
        Move-Item "$tmp\agentrouter-spoof-proxy-main" $InstallDir
        Remove-Item -Recurse -Force $tmp
      }
    }
  }
  Set-Location $InstallDir
}

# ── Pick method ──
if ($Docker) { $method = "docker" }
elseif ($PM2) { $method = "pm2" }
elseif ($Direct) { $method = "direct" }
elseif ($Yes) { $method = "docker" }
else {
  Write-Host ""
  Write-Host "How do you want to run the proxy?"
  Write-Host "  1) Docker Desktop  (recommended - isolated)"
  Write-Host "  2) PM2             (runs on host, auto-restart)"
  Write-Host "  3) Direct node     (foreground, for testing)"
  Write-Host ""
  $choice = Read-Host "Choice [1]"
  if ($choice -eq "") { $choice = "1" }
  switch ($choice) {
    "1" { $method = "docker" }
    "2" { $method = "pm2" }
    "3" { $method = "direct" }
    default { Write-Error "Invalid choice."; pause; exit 1 }
  }
}

# ── Setup .env ──
if (-not (Test-Path ".env")) {
  Copy-Item .env.example .env
  Write-OK ".env created (using defaults)"
}

# ── Method runners ──
switch ($method) {
  "docker" {
    if (-not $hasDocker) {
      Write-Warn "Docker not available."
      if (Confirm-Step "Install Docker Desktop") {
        Write-Info "Opening Docker Desktop download page..."
        Start-Process "https://desktop.docker.com/win/main/amd64/Docker%20Desktop%20Installer.exe"
        Write-Error "Install Docker Desktop first, then rerun the installer."
        pause; exit 1
      }
      Write-Error "Docker is required for this method. Choose PM2 or Direct."
      pause; exit 1
    }
    Run-Step "docker compose up -d --build" { docker compose up -d --build }
    if (-not $DryRun) { Start-Sleep -Seconds 5 }
    Write-Info "Checking health..."
    if ($DryRun) {
      Write-Info "DRY-RUN: check http://localhost:8318/health"
    } else {
      try {
        $health = Invoke-WebRequest -Uri http://localhost:8318/health -UseBasicParsing | ConvertFrom-Json
        Write-OK "Proxy running! http://localhost:8318"
        Write-Info "wafCookie=$($health.wafCookie) models=$($health.availableModels)"
      } catch {
        Write-Warn "Health check failed. Run: docker logs agentrouter-proxy"
      }
    }
  }

  "pm2" {
    try {
      pm2 --version | Out-Null
      Write-OK "PM2 available"
    } catch {
      Write-Info "Installing PM2..."
      Run-Step "npm install -g pm2" { npm install -g pm2 }
    }
    if (-not $DryRun) {
      pm2 start proxy.mjs --name agentrouter-proxy
      pm2 save
    } else {
      Write-Info "DRY-RUN: pm2 start proxy.mjs --name agentrouter-proxy"
      Write-Info "DRY-RUN: pm2 save"
    }
    Write-Info "Auto-start: pm2 startup (run as Admin)"
    if (-not $DryRun) { Start-Sleep -Seconds 3 }
    if ($DryRun) {
      Write-Info "DRY-RUN: check http://localhost:8318/health"
    } else {
      try {
        Invoke-WebRequest -Uri http://localhost:8318/health -UseBasicParsing | Out-Null
        Write-OK "Proxy running! http://localhost:8318"
      } catch {
        Write-Warn "WAF still warming up. Wait a few seconds."
      }
    }
    Write-Host ""
    Write-Info "Manage: pm2 status | pm2 logs agentrouter-proxy | pm2 restart agentrouter-proxy"
  }

  "direct" {
    Write-Info "Starting proxy in foreground (Ctrl+C to stop)..."
    Write-Host ""
    if ($DryRun) {
      Write-Info "DRY-RUN: node proxy.mjs"
    } else {
      node proxy.mjs
    }
  }
}

Write-Host ""
Write-OK "Done! Proxy is at http://localhost:8318"
Write-Host ""
Write-Info "Next steps:"
Write-Host "  - 9Router: Add custom OpenAI Compatible provider -> Base URL: http://localhost:8318/v1 -> Import from /models"
Write-Host "  - Direct:  curl http://localhost:8318/v1/messages -H 'Authorization: Bearer YOUR_KEY' -d '{\"model\":\"claude-opus-4-8\",\"max_tokens\":10,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
Write-Host "  - Docs:   https://github.com/trefeon/agentrouter-spoof-proxy/blob/main/docs/panduan-9router.md"
Write-Host ""
if (-not $Yes -and $method -ne "direct") { pause }
