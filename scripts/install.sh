#!/usr/bin/env bash
set -euo pipefail

# SECURITY NOTE: this installer downloads and executes remote code over TLS
# (GitHub releases, go.dev, get.docker.com). Review the script before piping it
# from the internet: https://github.com/trefeon/agentrouter-spoof-proxy/blob/main/scripts/install.sh

# AgentRouter Spoof Proxy - Linux One-Line Installer
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash -s -- --yes --docker
#   bash scripts/install.sh --dry-run --systemd

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

REPO_URL="https://github.com/trefeon/agentrouter-spoof-proxy.git"
TARBALL_URL="https://github.com/trefeon/agentrouter-spoof-proxy/archive/refs/heads/main.tar.gz"
RELEASES_URL="https://github.com/trefeon/agentrouter-spoof-proxy/releases"
DEFAULT_INSTALL_DIR="$HOME/agentrouter-spoof-proxy"

AGENTROUTER_VERSION="${AGENTROUTER_VERSION:-latest}"
GO_MIN_MAJOR=1
GO_MIN_MINOR=26
GO_MIN_VERSION="${GO_MIN_MAJOR}.${GO_MIN_MINOR}"

DRY_RUN=false
ASSUME_YES=false
METHOD=""
INSTALL_DIR="${AGENTROUTER_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"
PROJECT_DIR=""

info()  { printf "%b[INFO]%b  %s\n" "$CYAN" "$NC" "$*"; }
ok()    { printf "%b[OK]%b    %s\n" "$GREEN" "$NC" "$*"; }
warn()  { printf "%b[WARN]%b  %s\n" "$YELLOW" "$NC" "$*"; }
err()   { printf "%b[ERROR]%b %s\n" "$RED" "$NC" "$*"; }

usage() {
  cat <<'EOF'
AgentRouter Spoof Proxy installer

Options:
  --docker        Run with Docker Compose
  --systemd       Install as a systemd service on the host (--pm2 is a deprecated alias)
  --direct        Run the proxy binary in the foreground
  --yes, -y       Non-interactive: accept safe defaults and dependency installs
  --dry-run       Print what would happen without changing the system
  --dir PATH      Install/clone directory when run outside a repo
  --help, -h      Show this help

The proxy is a single static Go binary. A prebuilt binary is downloaded from
GitHub releases when available; otherwise it is built from source, which
requires Go 1.26+.

Environment:
  AGENTROUTER_INSTALL_DIR=/path  Default install directory for curl | bash
  AGENTROUTER_VERSION=latest     Prebuilt release version to download (default: latest)
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --docker) METHOD="docker" ;;
    --systemd) METHOD="systemd" ;;
    --pm2)
      METHOD="systemd"
      warn "--pm2 is deprecated; use --systemd (behavior unchanged)."
      ;;
    --direct) METHOD="direct" ;;
    --yes|-y) ASSUME_YES=true ;;
    --dry-run) DRY_RUN=true ;;
    --dir|--install-dir)
      [ "$#" -ge 2 ] || { err "--dir requires a path"; exit 2; }
      INSTALL_DIR="$2"
      shift
      ;;
    --help|-h) usage; exit 0 ;;
    *) err "Unknown option: $1"; usage; exit 2 ;;
  esac
  shift
done

run() {
  if $DRY_RUN; then
    info "DRY-RUN: $*"
  else
    "$@"
  fi
}

confirm() {
  local prompt="$1"
  if $ASSUME_YES; then
    return 0
  fi
  if [ ! -t 0 ]; then
    return 1
  fi
  local answer
  read -r -p "$prompt [y/N]: " answer
  case "$answer" in
    y|Y|yes|YES) return 0 ;;
    *) return 1 ;;
  esac
}

sudo_cmd() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    err "sudo is required for dependency installation. Install dependencies manually or run as root."
    return 1
  fi
}

detect_pm() {
  if command -v apt-get >/dev/null 2>&1; then echo apt; return; fi
  if command -v dnf >/dev/null 2>&1; then echo dnf; return; fi
  if command -v yum >/dev/null 2>&1; then echo yum; return; fi
  if command -v pacman >/dev/null 2>&1; then echo pacman; return; fi
  if command -v zypper >/dev/null 2>&1; then echo zypper; return; fi
  echo unknown
}

install_packages() {
  local pm="$1"
  shift
  [ "$#" -gt 0 ] || return 0

  if ! confirm "Install missing dependency packages: $*"; then
    return 1
  fi

  case "$pm" in
    apt)
      run sudo_cmd apt-get update
      run sudo_cmd apt-get install -y "$@"
      ;;
    dnf) run sudo_cmd dnf install -y "$@" ;;
    yum) run sudo_cmd yum install -y "$@" ;;
    pacman) run sudo_cmd pacman -Sy --needed --noconfirm "$@" ;;
    zypper) run sudo_cmd zypper --non-interactive install "$@" ;;
    *)
      err "Unsupported package manager. Missing packages: $*"
      return 1
      ;;
  esac
}

ensure_command() {
  local cmd="$1"
  shift
  if command -v "$cmd" >/dev/null 2>&1; then
    return 0
  fi
  warn "$cmd not found."
  install_packages "$(detect_pm)" "$@"
}

detect_arch() {
  local m
  m="$(uname -m 2>/dev/null || echo unknown)"
  case "$m" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *)
      warn "Unknown architecture '$m'; assuming amd64."
      echo amd64
      ;;
  esac
}

# Download the prebuilt binary from GitHub releases (Step A).
download_prebuilt() {
  local dest="$1" os_arch url
  case "$(detect_arch)" in
    amd64) os_arch="linux-amd64" ;;
    arm64) os_arch="linux-arm64" ;;
    *) err "Unsupported architecture: $(uname -m)"; return 1 ;;
  esac
  url="$RELEASES_URL/${AGENTROUTER_VERSION}/download/agentrouter-proxy-${os_arch}"
  info "Downloading prebuilt binary: $url"
  if $DRY_RUN; then
    info "DRY-RUN: curl -fsSL '$url' -o '$dest'"
    return 0
  fi
  ensure_command curl curl || return 1
  if ! curl -fsSL "$url" -o "$dest"; then
    warn "Prebuilt binary download failed (no release published yet is normal)."
    return 1
  fi
  chmod +x "$dest"
  ok "Downloaded agentrouter-proxy binary"
  return 0
}

# Install Go 1.26+ from the official go.dev tarball (preferred when the
# package manager ships an old golang-go).
install_go_tarball() {
  local arch url tmp
  arch=$(detect_arch)
  url="https://go.dev/dl/go${GO_MIN_VERSION}.linux-${arch}.tar.gz"
  info "Downloading Go $GO_MIN_VERSION from go.dev: $url"
  if $DRY_RUN; then
    info "DRY-RUN: curl -fsSL '$url' -o /tmp/go.tar.gz && sudo tar -C /usr/local -xzf /tmp/go.tar.gz && export PATH=/usr/local/go/bin:\$PATH"
    return 0
  fi
  tmp="$(mktemp -d)"
  if ! curl -fsSL "$url" -o "$tmp/go.tar.gz"; then
    rm -rf "$tmp"
    return 1
  fi
  run sudo_cmd rm -rf /usr/local/go
  run sudo_cmd tar -C /usr/local -xzf "$tmp/go.tar.gz"
  rm -rf "$tmp"
  export PATH="/usr/local/go/bin:$PATH"
  ok "Installed Go $GO_MIN_VERSION to /usr/local/go"
  return 0
}

# Ensure a Go toolchain >= 1.26 is available (Step B prerequisite).
ensure_go() {
  if command -v go >/dev/null 2>&1; then
    local ver major minor
    ver=$(go version | sed -E 's/.*go([0-9]+)\.([0-9]+).*/\1.\2/')
    major="${ver%%.*}"
    minor="${ver##*.}"
    if [ "$major" -gt "$GO_MIN_MAJOR" ] || { [ "$major" -eq "$GO_MIN_MAJOR" ] && [ "$minor" -ge "$GO_MIN_MINOR" ]; }; then
      ok "Go $ver"
      return 0
    fi
    warn "Go $ver found. Go $GO_MIN_VERSION+ is required to build from source."
  else
    warn "Go not found."
  fi

  local pm
  pm=$(detect_pm)
  case "$pm" in
    apt)
      # apt's golang-go can lag behind 1.26; prefer the official tarball.
      if confirm "Install Go $GO_MIN_VERSION+ from the official tarball (go.dev)"; then
        install_go_tarball && return 0
      fi
      warn "The apt golang-go package may be older than $GO_MIN_VERSION."
      if confirm "Install Go from the system package manager anyway"; then
        run sudo_cmd apt-get update
        run sudo_cmd apt-get install -y golang-go
        return 0
      fi
      ;;
    dnf|yum|pacman|zypper)
      install_packages "$pm" golang && return 0
      ;;
  esac

  err "Go $GO_MIN_VERSION+ is required to build from source."
  err "Install it manually (https://go.dev/dl/) and rerun, or wait for a prebuilt release."
  return 1
}

# Build the binary from source in PROJECT_DIR (Step B).
build_from_source() {
  local dest="$1"
  local src="${PROJECT_DIR:-$INSTALL_DIR}"

  if $DRY_RUN; then
    info "DRY-RUN: go build -trimpath -ldflags=\"-s -w\" -o $dest ./cmd/proxy  (in $src)"
    return 0
  fi

  if [ ! -f "$src/go.mod" ] || [ ! -d "$src/cmd/proxy" ]; then
    warn "No Go source tree at $src; cloning the repository for the build."
    if [ -d "$INSTALL_DIR" ] && [ -n "$(ls -A "$INSTALL_DIR" 2>/dev/null)" ]; then
      confirm "Directory $INSTALL_DIR is not empty. Remove it and clone fresh?" || { err "Aborting: $INSTALL_DIR is not empty."; exit 1; }
      run rm -rf "$INSTALL_DIR"
    fi
    run git clone "$REPO_URL" "$INSTALL_DIR"
    src="$INSTALL_DIR"
  fi

  info "Building agentrouter-proxy from source (Go toolchain)..."
  if ! (cd "$src" && go build -trimpath -ldflags="-s -w" -o "$dest" ./cmd/proxy); then
    err "go build failed."
    return 1
  fi
  ok "Built agentrouter-proxy from source"
  return 0
}

# Step A -> B -> C: download prebuilt, fall back to source build.
obtain_binary() {
  local dest="$1"
  download_prebuilt "$dest"
  if $DRY_RUN; then
    info "DRY-RUN: fallback if download fails -> go build -trimpath -ldflags=\"-s -w\" -o $dest ./cmd/proxy (Go $GO_MIN_VERSION+ required)"
    return 0
  fi
  warn "Prebuilt binary unavailable; falling back to building from source."
  ensure_go || return 1
  build_from_source "$dest"
}

ensure_docker() {
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    if docker compose version >/dev/null 2>&1; then
      ok "Docker available"
      return 0
    fi
    warn "Docker daemon is running but the compose plugin is missing."
    if install_packages "$(detect_pm)" docker-compose-plugin; then
      return 0
    fi
    return 1
  fi

  warn "Docker is not available or the daemon is not running."
  local pm
  pm=$(detect_pm)
  case "$pm" in
    apt)
      if confirm "Install Docker using Docker's official convenience script"; then
        run sudo_cmd apt-get update
        run sudo_cmd apt-get install -y ca-certificates curl
        if $DRY_RUN; then
          info "DRY-RUN: curl https://get.docker.com | sh"
        else
          curl -fsSL https://get.docker.com | sudo sh
          sudo usermod -aG docker "$(id -un)" || true
          warn "If Docker was just installed, log out/in or run: newgrp docker"
        fi
        return 0
      fi
      ;;
    dnf|yum|pacman|zypper)
      install_packages "$pm" docker docker-compose-plugin && return 0
      ;;
  esac

  return 1
}

resolve_project_dir() {
  if [ -f go.mod ] && [ -d cmd/proxy ]; then
    PROJECT_DIR="$(pwd)"
    return 0
  fi

  local script_dir=""
  if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || true)"
  fi
  if [ -n "$script_dir" ] && [ -f "$script_dir/../go.mod" ]; then
    PROJECT_DIR="$(cd "$script_dir/.." && pwd)"
    return 0
  fi

  info "Project files not found in current directory. Installing to: $INSTALL_DIR"
  if [ -d "$INSTALL_DIR/.git" ]; then
    PROJECT_DIR="$INSTALL_DIR"
    info "Existing git checkout found; pulling latest."
    run git -C "$PROJECT_DIR" pull --ff-only
    return 0
  fi
  if [ -f "$INSTALL_DIR/go.mod" ]; then
    PROJECT_DIR="$INSTALL_DIR"
    return 0
  fi

  run mkdir -p "$(dirname "$INSTALL_DIR")"
  if command -v git >/dev/null 2>&1; then
    if [ -d "$INSTALL_DIR" ] && [ -n "$(ls -A "$INSTALL_DIR" 2>/dev/null)" ]; then
      confirm "Directory $INSTALL_DIR is not empty. Remove it and clone fresh?" || { err "Aborting: $INSTALL_DIR is not empty."; exit 1; }
      run rm -rf "$INSTALL_DIR"
    fi
    run git clone "$REPO_URL" "$INSTALL_DIR"
    PROJECT_DIR="$INSTALL_DIR"
    return 0
  fi

  ensure_command curl curl
  ensure_command tar tar
  info "git not found; downloading source tarball."
  if $DRY_RUN; then
    info "DRY-RUN: download and extract $TARBALL_URL to $INSTALL_DIR"
  else
    local tmp top=""
    tmp="$(mktemp -d)"
    curl -fsSL "$TARBALL_URL" -o "$tmp/source.tar.gz"
    tar -xzf "$tmp/source.tar.gz" -C "$tmp"
    for entry in $(tar -tzf "$tmp/source.tar.gz" 2>/dev/null); do
      top="${entry%%/*}"
      break
    done
    [ -n "$top" ] || { err "Cannot determine archive root directory"; rm -rf "$tmp"; exit 1; }
    if [ -d "$INSTALL_DIR" ] && [ -n "$(ls -A "$INSTALL_DIR" 2>/dev/null)" ]; then
      confirm "Remove existing non-empty directory $INSTALL_DIR and install fresh?" || { err "Aborting: $INSTALL_DIR is not empty."; exit 1; }
    fi
    rm -rf "$INSTALL_DIR"
    mv "$tmp/$top" "$INSTALL_DIR"
    rm -rf "$tmp"
  fi
  PROJECT_DIR="$INSTALL_DIR"
}

pick_method() {
  if [ -n "$METHOD" ]; then
    return 0
  fi
  if [ ! -t 0 ]; then
    METHOD="docker"
    return 0
  fi

  echo ""
  echo "How do you want to run the proxy?"
  echo "  1) Docker          (recommended - auto-restart, isolated)"
  echo "  2) systemd         (runs on host, auto-restart)"
  echo "  3) Direct binary   (foreground, for testing only)"
  echo ""
  local choice
  read -r -p "Choice [1]: " choice
  choice=${choice:-1}
  case "$choice" in
    1) METHOD="docker" ;;
    2) METHOD="systemd" ;;
    3) METHOD="direct" ;;
    *) err "Invalid choice."; exit 1 ;;
  esac
}

ensure_env() {
  if [ ! -f .env ]; then
    run cp .env.example .env
    ok ".env created from .env.example"
  fi
}

health_check() {
  local url="http://127.0.0.1:8318/health"
  if $DRY_RUN; then
    info "DRY-RUN: poll $url up to 15s; expect HTTP 200 with {\"ok\":true}"
    return 0
  fi
  info "Waiting for proxy health (up to 15s)..."
  local i
  for i in $(seq 1 30); do
    if curl -fsS "$url" 2>/dev/null | grep -q '"ok":true'; then
      ok "Proxy running! http://localhost:8318"
      info "Health: $(curl -s "$url")"
      return 0
    fi
    sleep 0.5
  done
  err "Health check failed after 15s."
  if command -v curl >/dev/null 2>&1; then
    err "Last health payload: $(curl -s "$url" || echo unavailable)"
  fi
  err "Hint: check the proxy logs. systemd: systemctl status agentrouter-proxy; journalctl -u agentrouter-proxy -n 50. Docker: docker logs agentrouter-proxy."
  return 1
}

run_docker() {
  if ! ensure_docker; then
    warn "Docker unavailable. Falling back to systemd."
    METHOD="systemd"
    run_systemd || return 1
    return
  fi
  ensure_env
  info "Starting with Docker Compose..."
  run docker compose up -d --build
  if ! $DRY_RUN; then
    sleep 5
  fi
  health_check || return 1
}

run_systemd() {
  if ! $DRY_RUN && ! command -v systemctl >/dev/null 2>&1; then
    err "systemctl not found; systemd is required for this method."
    err "Use --docker instead, or run --direct in the foreground."
    exit 1
  fi

  ensure_env

  local bin="/usr/local/bin/agentrouter-proxy"
  local env_file="/etc/agentrouter-proxy.env"
  local unit_src="$PROJECT_DIR/deploy/agentrouter-proxy.service"
  local unit_dest="/etc/systemd/system/agentrouter-proxy.service"

  info "Installing agentrouter-proxy as a systemd service..."

  # 1) Obtain and install the binary
  if $DRY_RUN; then
    obtain_binary "$bin"
  else
    local tmp_bin
    tmp_bin="$(mktemp)"
    if ! obtain_binary "$tmp_bin"; then
      err "Could not obtain the agentrouter-proxy binary."
      err "Install Go $GO_MIN_VERSION+ and rerun, or check for prebuilt releases: $RELEASES_URL"
      exit 1
    fi
    run sudo_cmd install -m 0755 "$tmp_bin" "$bin"
    rm -f "$tmp_bin"
    ok "Installed binary to $bin"
  fi

  # 2) Environment file (/etc/agentrouter-proxy.env from the user's .env)
  if [ -f .env ] || $DRY_RUN; then
    run sudo_cmd cp .env "$env_file"
    ok "Installed environment to $env_file"
  else
    err "No .env found (ensure_env should have created one)."
    exit 1
  fi

  # 3) systemd unit
  if [ -f "$unit_src" ]; then
    run sudo_cmd cp "$unit_src" "$unit_dest"
  elif $DRY_RUN; then
    info "DRY-RUN: sudo cp <repo>/deploy/agentrouter-proxy.service $unit_dest"
  else
    err "Missing unit file: $unit_src"
    exit 1
  fi
  run sudo_cmd systemctl daemon-reload
  run sudo_cmd systemctl enable --now agentrouter-proxy
  ok "agentrouter-proxy service installed, enabled and started"

  if ! $DRY_RUN; then
    sleep 3
  fi
  health_check || return 1
  info "Manage: systemctl status agentrouter-proxy | systemctl restart agentrouter-proxy | journalctl -u agentrouter-proxy -f"
}

run_direct() {
  ensure_env
  local bin="$PROJECT_DIR/agentrouter-proxy"
  if ! obtain_binary "$bin"; then
    err "Could not obtain the agentrouter-proxy binary."
    err "Install Go $GO_MIN_VERSION+ and rerun, or check for prebuilt releases: $RELEASES_URL"
    exit 1
  fi
  if ! $DRY_RUN && [ ! -x "$bin" ]; then
    err "Binary not found or not executable: $bin"
    exit 1
  fi
  info "Starting proxy in foreground (Ctrl+C to stop)..."
  if $DRY_RUN; then
    info "DRY-RUN: ./agentrouter-proxy"
  else
    cd "$PROJECT_DIR" && ./agentrouter-proxy
  fi
}

echo ""
info "AgentRouter Spoof Proxy Installer"
if $DRY_RUN; then warn "Dry-run mode enabled; no changes will be made."; fi

resolve_project_dir
if [ -d "$PROJECT_DIR" ]; then
  cd "$PROJECT_DIR"
elif $DRY_RUN; then
  info "DRY-RUN: project would be at $PROJECT_DIR (not created in dry-run)"
else
  err "Project directory $PROJECT_DIR is missing."
  exit 1
fi
info "Project dir: $PROJECT_DIR"

pick_method
case "$METHOD" in
  docker) run_docker || exit 1 ;;
  systemd) run_systemd || exit 1 ;;
  direct) run_direct ;;
  *) err "Unknown method: $METHOD"; exit 1 ;;
esac

echo ""
ok "Done! Proxy is at http://localhost:8318"
echo ""
info "Next steps:"
echo "  - 9Router:      Add custom OpenAI Compatible provider -> Base URL: http://localhost:8318/v1 -> Import models from /models"
echo "  - Direct test:  curl http://localhost:8318/v1/messages -H 'Authorization: Bearer YOUR_KEY' -H 'Content-Type: application/json' -d '{\"model\":\"claude-opus-4-8\",\"max_tokens\":10,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
echo "  - Docs:         https://github.com/trefeon/agentrouter-spoof-proxy/blob/main/docs/panduan-9router.md"
echo ""
