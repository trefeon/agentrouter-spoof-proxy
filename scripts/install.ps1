# AgentRouter Spoof Proxy — Windows One-Click Installer
# SECURITY NOTE: this installer downloads and executes remote code over TLS
# (GitHub releases, go.dev, Docker Desktop). Review the script before piping it
# from the internet: https://github.com/trefeon/agentrouter-spoof-proxy/blob/main/scripts/install.ps1
#
# Usage:
#   iwr -useb https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.ps1 | iex
#     (interactive - prompts for run method; parameters cannot be passed through the pipe)
#   Invoke-WebRequest -Uri <script-url> -OutFile install.ps1; .\install.ps1 -Docker -Yes
#   powershell -ExecutionPolicy Bypass -File install.ps1 -Service -DryRun
#
# Parameters:
#   -Docker         Run with Docker Compose
#   -Service        Install as a Windows service (sc.exe create; -PM2 is a deprecated alias)
#   -Direct         Run the proxy binary in the foreground
#   -Yes            Non-interactive mode
#   -DryRun         Print what would happen without changing system
#   -InstallDir     Install directory (default: ~\agentrouter-spoof-proxy)
#
# The proxy is a single static Go binary. A prebuilt binary is downloaded from
# GitHub releases when available; otherwise it is built from source, which
# requires Go 1.26+.

param(
  [switch]$Docker,
  [switch]$Service,
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
$releasesUrl = "https://github.com/trefeon/agentrouter-spoof-proxy/releases"
$goMinVersion = "1.26"
$version = if ($env:AGENTROUTER_VERSION) { $env:AGENTROUTER_VERSION } else { "latest" }
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

# ── Go toolchain detection (1.26+ required for source builds) ──
function Test-Go {
  try {
    $goVer = (go version) -replace '.*go(\d+)\.(\d+).*', '$1.$2'
    $parts = $goVer -split '\.'
    if ([int]$parts[0] -lt 1 -or ([int]$parts[0] -eq 1 -and [int]$parts[1] -lt 26)) {
      Write-Warn "Go $goVer found. Go $goMinVersion+ is required to build from source."
      return $false
    }
    Write-OK "Go $goVer"
    return $true
  } catch { return $false }
}

function Install-Go {
  if ($Yes) {
    Write-Info "Attempting install via winget..."
    Run-Step "winget install GoLang.Go" { winget install GoLang.Go -e --accept-source-agreements --accept-package-agreements }
    # winget updates the registry PATH; the current session's $env:Path is stale.
    $env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
    if (-not (Test-Go)) {
      Write-Fail "Go was installed but is not on PATH yet. Open a new terminal and rerun the installer."
      return $false
    }
    return $true
  }
  Write-Host "  Install: https://go.dev/dl/ ($goMinVersion+)"
  Write-Host "  Or: winget install GoLang.Go"
  return $false
}

# Build the binary from source in $InstallDir (Step B).
function Build-Source {
  param([string]$Dest)
  $src = $InstallDir
  if ($DryRun) {
    Write-Info "DRY-RUN: go build -trimpath -ldflags=`"-s -w`" -o $Dest ./cmd/proxy  (in $src)"
    return $true
  }
  if (-not (Test-Path (Join-Path $src "go.mod")) -or -not (Test-Path (Join-Path $src "cmd\proxy"))) {
    Write-Warn "No Go source tree at $src; cloning the repository for the build."
    if (Test-Path $src) {
      if ((Get-ChildItem -Force $src | Measure-Object).Count -gt 0) {
        if (Confirm-Step "Directory $src is not empty. Remove it and clone fresh?") {
          Run-Step "Remove existing $src" { Remove-Item -Recurse -Force $src }
        } else {
          Write-Fail "Aborting: $src is not empty."
          pause; exit 1
        }
      }
    }
    Run-Step "git clone $repo $src" { git clone $repo $src }
  }
  Write-Info "Building agentrouter-proxy from source (Go toolchain)..."
  Push-Location $src
  try {
    & go build -trimpath -ldflags="-s -w" -o $Dest ./cmd/proxy
    if ($LASTEXITCODE -ne 0) {
      Write-Fail "go build failed."
      return $false
    }
  } finally {
    Pop-Location
  }
  Write-OK "Built agentrouter-proxy from source"
  return $true
}

# Step A -> B -> C: download prebuilt, fall back to source build.
function Get-Binary {
  param([string]$Dest)
  $url = "$releasesUrl/$version/download/agentrouter-proxy.exe"
  Write-Info "Downloading prebuilt binary: $url"
  if ($DryRun) {
    Write-Info "DRY-RUN: Invoke-WebRequest -Uri $url -OutFile $Dest"
    Write-Info "DRY-RUN: fallback if download fails -> go build -trimpath -ldflags=`"-s -w`" -o $Dest ./cmd/proxy (Go $goMinVersion+ required)"
    return $true
  }
  try {
    Invoke-WebRequest -Uri $url -OutFile $Dest -UseBasicParsing
    Write-OK "Downloaded prebuilt binary"
    return $true
  } catch {
    Write-Warn "Prebuilt binary download failed (no release published yet is normal)."
  }
  Write-Warn "Falling back to building from source."
  if (-not (Test-Go)) {
    if (-not (Install-Go)) { return $false }
  }
  return (Build-Source $Dest)
}

# ── Health verification: poll up to ~15s for {"ok":true} ──
function Test-Health {
  if ($DryRun) {
    Write-Info "DRY-RUN: poll http://127.0.0.1:8318/health up to 15s; expect ok=true"
    return $true
  }
  Write-Info "Waiting for proxy health (up to 15s)..."
  for ($i = 0; $i -lt 30; $i++) {
    try {
      $h = Invoke-RestMethod -Uri http://127.0.0.1:8318/health -TimeoutSec 2
      if ($h.ok -eq $true) {
        Write-OK "Proxy running! http://localhost:8318"
        Write-Info "wafCookie=$($h.wafCookie) models=$($h.availableModels)"
        return $true
      }
    } catch { }
    Start-Sleep -Milliseconds 500
  }
  Write-Fail "Health check failed after 15s."
  try {
    $payload = (Invoke-WebRequest -Uri http://127.0.0.1:8318/health -UseBasicParsing -TimeoutSec 2).Content
    Write-Fail "Health payload: $payload"
  } catch { }
  Write-Fail "Hint: check the service (Get-Service agentrouter-proxy) or run .\agentrouter-proxy.exe in the foreground to see logs."
  return $false
}

function Test-Admin {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  $p = New-Object Security.Principal.WindowsPrincipal($id)
  return $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

Write-Info "AgentRouter Spoof Proxy — Windows Installer"
Write-Info "--------------------------------------------"
Write-Host ""
if ($DryRun) { Write-Warn "Dry-run mode enabled; no changes will be made." }

if ($Help) {
  Write-Host "AgentRouter Spoof Proxy installer"
  Write-Host ""
  Write-Host "Usage: .\install.ps1 [-Docker | -Service | -Direct] [-Yes] [-DryRun] [-InstallDir <path>] [-Help]"
  Write-Host ""
  Write-Host "  -Docker       Run with Docker Compose"
  Write-Host "  -Service      Install as a Windows service (sc.exe create; -PM2 is a deprecated alias)"
  Write-Host "  -Direct       Run the proxy binary in the foreground"
  Write-Host "  -Yes          Non-interactive mode"
  Write-Host "  -DryRun       Print what would happen without changing the system"
  Write-Host "  -InstallDir   Install directory (default: ~\agentrouter-spoof-proxy)"
  Write-Host ""
  Write-Host "The proxy is a single static Go binary. A prebuilt binary is downloaded from"
  Write-Host "GitHub releases when available; otherwise it is built from source, which"
  Write-Host "requires Go 1.26+."
  Write-Host "Environment: AGENTROUTER_VERSION (default: latest)"
  exit 0
}

# ── Detect Docker (optional) ──
$hasDocker = $false
try {
  docker --version | Out-Null
  docker info | Out-Null
  $hasDocker = $true
} catch {}

# ── Clone or download repo ──
if ((Test-Path (Join-Path $InstallDir "go.mod")) -and (Test-Path (Join-Path $InstallDir "cmd\proxy"))) {
  Write-Info "Found project at $InstallDir"
  Set-Location $InstallDir
  if (Test-Path ".git") {
    Run-Step "git pull --ff-only" { git pull --ff-only }
  }
} elseif (Test-Path "go.mod") {
  $InstallDir = (Get-Location).Path
} else {
  Write-Info "Installing to $InstallDir"

  if (-not (Test-Path (Join-Path $InstallDir "go.mod"))) {
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
elseif ($Service -or $PM2) {
  if ($PM2 -and -not $Service) { Write-Warn "-PM2 is deprecated; use -Service instead (behavior unchanged)." }
  $method = "service"
}
elseif ($Direct) { $method = "direct" }
elseif ($Yes) { $method = "docker" }
else {
  Write-Host ""
  Write-Host "How do you want to run the proxy?"
  Write-Host "  1) Docker Desktop  (recommended - isolated)"
  Write-Host "  2) Service         (Windows service, auto-restart)"
  Write-Host "  3) Direct binary   (foreground, for testing)"
  Write-Host ""
  $choice = Read-Host "Choice [1]"
  if ($choice -eq "") { $choice = "1" }
  switch ($choice) {
    "1" { $method = "docker" }
    "2" { $method = "service" }
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
      Write-Fail "Docker is required for this method. Choose Service or Direct."
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
    if (-not (Test-Health)) { exit 1 }
  }

  "service" {
    if (-not $DryRun -and -not (Test-Admin)) {
      Write-Fail "Administrator privileges are required to register a Windows service."
      Write-Fail "Rerun the installer from an elevated PowerShell (Run as Administrator)."
      pause; exit 1
    }
    $binPath = Join-Path $InstallDir "agentrouter-proxy.exe"
    if (-not (Get-Binary $binPath)) {
      Write-Fail "Could not obtain the agentrouter-proxy binary."
      Write-Fail "Install Go $goMinVersion+ and rerun, or check for prebuilt releases: $releasesUrl"
      pause; exit 1
    }
    Write-Info "Registering Windows service 'agentrouter-proxy'..."
    if ($DryRun) {
      Write-Info "DRY-RUN: sc.exe create agentrouter-proxy binPath= `"$binPath`" start= auto"
      Write-Info "DRY-RUN: sc.exe start agentrouter-proxy"
    } else {
      Run-Step "sc.exe create agentrouter-proxy" { sc.exe create agentrouter-proxy binPath= "`"$binPath`"" start= auto }
      Run-Step "sc.exe start agentrouter-proxy" { sc.exe start agentrouter-proxy }
      Write-OK "Service 'agentrouter-proxy' registered (start= auto)"
    }
    if (-not $DryRun) { Start-Sleep -Seconds 3 }
    if (-not (Test-Health)) { exit 1 }
    Write-Host ""
    Write-Info "Manage: Get-Service agentrouter-proxy | Start-Service agentrouter-proxy | Stop-Service agentrouter-proxy | sc.exe delete agentrouter-proxy"
  }

  "direct" {
    $binPath = Join-Path $InstallDir "agentrouter-proxy.exe"
    if (-not (Get-Binary $binPath)) {
      Write-Fail "Could not obtain the agentrouter-proxy binary."
      Write-Fail "Install Go $goMinVersion+ and rerun, or check for prebuilt releases: $releasesUrl"
      pause; exit 1
    }
    Write-Info "Starting proxy in foreground (Ctrl+C to stop)..."
    Write-Host ""
    if ($DryRun) {
      Write-Info "DRY-RUN: .\agentrouter-proxy.exe"
    } else {
      & $binPath
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
