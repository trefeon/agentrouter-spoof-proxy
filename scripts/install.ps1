# AgentRouter Spoof Proxy — Windows One-Click Installer
# SECURITY NOTE: this installer downloads and executes remote code over TLS
# (GitHub, NodeSource, Docker Desktop). Review the script before piping it
# from the internet: https://github.com/trefeon/agentrouter-spoof-proxy/blob/main/scripts/install.ps1
#
# Usage:
#   iwr -useb https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.ps1 | iex
#     (interactive - prompts for run method; parameters cannot be passed through the pipe)
#   Invoke-WebRequest -Uri <script-url> -OutFile install.ps1; .\install.ps1 -Docker -Yes
#   powershell -ExecutionPolicy Bypass -File install.ps1 -PM2 -DryRun
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
  [switch]$Help,
  [string]$InstallDir = "$env:USERPROFILE\agentrouter-spoof-proxy"
)

$ErrorActionPreference = "Stop"
$repo = "https://github.com/trefeon/agentrouter-spoof-proxy.git"
$tarballUrl = "https://github.com/trefeon/agentrouter-spoof-proxy/archive/refs/heads/main.tar.gz"
$method = ""

function Write-Info  { Write-Host "[INFO]  $args" -ForegroundColor Cyan }
function Write-OK    { Write-Host "[OK]    $args" -ForegroundColor Green }
function Write-Warn  { Write-Host "[WARN]  $args" -ForegroundColor Yellow }
function Write-Fail { Write-Host "[ERROR] $args" -ForegroundColor Red }

function Run-Step {
  param([string]$Desc, [scriptblock]$Block)
  if ($DryRun) {
    Write-Info "DRY-RUN: $Desc"
    return
  }
  Write-Info "Running: $Desc"
  $LASTEXITCODE = 0
  & $Block
  if ($LASTEXITCODE -ne 0) { throw "$Desc failed with exit code $LASTEXITCODE" }
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

if ($Help) {
  Write-Host "AgentRouter Spoof Proxy installer"
  Write-Host ""
  Write-Host "Usage: .\install.ps1 [-Docker | -PM2 | -Direct] [-Yes] [-DryRun] [-InstallDir <path>] [-Help]"
  Write-Host ""
  Write-Host "  -Docker       Run with Docker Compose"
  Write-Host "  -PM2          Run with PM2 on host"
  Write-Host "  -Direct       Run with node in foreground"
  Write-Host "  -Yes          Non-interactive mode"
  Write-Host "  -DryRun       Print what would happen without changing the system"
  Write-Host "  -InstallDir   Install directory (default: ~\agentrouter-spoof-proxy)"
  exit 0
}

# ── Detect Node.js ──
function Test-Node {
  try {
    $nodeVer = (node -v) -replace '^v',''
    $verParts = $nodeVer -split '\.'
    if ([int]$verParts[0] -lt 22) {
      Write-Warn "Node.js $nodeVer found. Recommended 22+. Continuing anyway."
    } else {
      Write-OK "Node.js $nodeVer"
    }
    return $true
  } catch { return $false }
}

$nodeOk = Test-Node
if (-not $nodeOk) {
  Write-Warn "Node.js not found."
  if ($Yes) {
    Write-Info "Attempting install via winget..."
    Run-Step "winget install OpenJS.NodeJS.LTS" { winget install OpenJS.NodeJS.LTS -e --accept-source-agreements --accept-package-agreements }
    # winget updates the registry PATH; the current session's $env:Path is stale.
    $env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
    $nodeOk = Test-Node
    if (-not $nodeOk) {
      Write-Fail "Node.js was installed but is not on PATH yet. Open a new terminal and rerun the installer."
      pause; exit 1
    }
  } else {
    Write-Host "  Install: https://nodejs.org (LTS, 22+)"
    Write-Host "  Or: winget install OpenJS.NodeJS.LTS"
  }
}
if (-not $nodeOk -and -not $DryRun) {
  Write-Fail "Node.js is required."
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
  if (Test-Path ".git") {
    Run-Step "git pull --ff-only" { git pull --ff-only }
  }
} elseif (Test-Path "proxy.mjs") {
  $InstallDir = (Get-Location).Path
} else {
  Write-Info "Installing to $InstallDir"

  if (-not (Test-Path (Join-Path $InstallDir "proxy.mjs"))) {
    if (Test-Path $InstallDir) {
      if ((Get-ChildItem -Force $InstallDir | Measure-Object).Count -gt 0) {
        if (Confirm-Step "Directory $InstallDir is not empty. Remove it and install fresh?") {
          Run-Step "Remove existing $InstallDir" { Remove-Item -Recurse -Force $InstallDir }
        } else {
          Write-Fail "Aborting: $InstallDir is not empty."
          pause; exit 1
        }
      }
    }

    try { git --version | Out-Null; $hasGit = $true } catch { $hasGit = $false }

    if ($hasGit) {
      Run-Step "git clone $repo $InstallDir" { git clone $repo $InstallDir }
    } else {
      Write-Warn "Git not found. Downloading tarball instead..."
      $tmp = "$env:TEMP\agentrouter-install"
      Run-Step "Download and extract tarball to $tmp" {
        New-Item -ItemType Directory -Force -Path $tmp | Out-Null
        Invoke-WebRequest -Uri $tarballUrl -OutFile "$tmp\source.tar.gz"
        tar -xzf "$tmp\source.tar.gz" -C $tmp
      }
      if (-not $DryRun) {
        $top = (tar -tzf "$tmp\source.tar.gz" | Select-Object -First 1)
        if (-not $top) { throw "Cannot determine archive root directory" }
        $top = $top -replace '/.*$',''
        if (Test-Path $InstallDir) { Remove-Item -Recurse -Force $InstallDir }
        Move-Item "$tmp\$top" $InstallDir
        Remove-Item -Recurse -Force $tmp
      }
    }
  }

  if ($DryRun -and -not (Test-Path $InstallDir)) {
    Write-Info "DRY-RUN: project would be at $InstallDir (directory not created in dry-run)"
  } else {
    Set-Location $InstallDir
  }
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
    default { Write-Fail "Invalid choice."; pause; exit 1 }
  }
}

# ── Setup .env ──
if (-not (Test-Path ".env")) {
  if ($DryRun) {
    Write-Info "DRY-RUN: Copy-Item .env.example .env"
  } else {
    Copy-Item .env.example .env
    Write-OK ".env created (using defaults)"
  }
}

# ── Method runners ──
switch ($method) {
  "docker" {
    if (-not $hasDocker) {
      Write-Warn "Docker not available."
      if (Confirm-Step "Install Docker Desktop") {
        Write-Info "Opening Docker Desktop download page..."
        Start-Process "https://desktop.docker.com/win/main/amd64/Docker%20Desktop%20Installer.exe"
        Write-Fail "Install Docker Desktop first, then rerun the installer."
        pause; exit 1
      }
      Write-Fail "Docker is required for this method. Choose PM2 or Direct."
      pause; exit 1
    }
    # Native exit codes do not throw in PowerShell — check $LASTEXITCODE explicitly.
    docker compose version | Out-Null
    if ($LASTEXITCODE -ne 0) {
      Write-Fail "Docker is installed but the compose plugin is missing. Update Docker Desktop (Compose v2 is bundled)."
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
    Run-Step "pm2 start proxy.mjs --name agentrouter-proxy" { pm2 start proxy.mjs --name agentrouter-proxy }
    Run-Step "pm2 save" { pm2 save }
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
