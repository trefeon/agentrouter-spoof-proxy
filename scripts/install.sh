#!/usr/bin/env bash
set -euo pipefail

# SECURITY NOTE: this installer downloads and executes remote code over TLS
# (GitHub, NodeSource, get.docker.com). Review the script before piping it
# from the internet: https://github.com/trefeon/agentrouter-spoof-proxy/blob/main/scripts/install.sh

# AgentRouter Spoof Proxy - Linux One-Line Installer
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash -s -- --yes --docker
#   bash scripts/install.sh --dry-run --docker

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

REPO_URL="https://github.com/trefeon/agentrouter-spoof-proxy.git"
TARBALL_URL="https://github.com/trefeon/agentrouter-spoof-proxy/archive/refs/heads/main.tar.gz"
DEFAULT_INSTALL_DIR="$HOME/agentrouter-spoof-proxy"

DRY_RUN=false
ASSUME_YES=false
METHOD=""
INSTALL_DIR="${AGENTROUTER_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"

info()  { printf "%b[INFO]%b  %s\n" "$CYAN" "$NC" "$*"; }
ok()    { printf "%b[OK]%b    %s\n" "$GREEN" "$NC" "$*"; }
warn()  { printf "%b[WARN]%b  %s\n" "$YELLOW" "$NC" "$*"; }
err()   { printf "%b[ERROR]%b %s\n" "$RED" "$NC" "$*"; }

usage() {
  cat <<'EOF'
AgentRouter Spoof Proxy installer

Options:
  --docker        Run with Docker Compose
  --pm2           Run with PM2 on the host
  --direct        Run with node in the foreground
  --yes, -y       Non-interactive: accept safe defaults and dependency installs
  --dry-run       Print what would happen without changing the system
  --dir PATH      Install/clone directory when run outside a repo
  --help, -h      Show this help

Environment:
  AGENTROUTER_INSTALL_DIR=/path  Default install directory for curl | bash
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --docker) METHOD="docker" ;;
    --pm2) METHOD="pm2" ;;
    --direct) METHOD="direct" ;;
    --yes|-y) ASSUME_YES=true ;;
    --dry-run) DRY_RUN=true ;;
    --dir)
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

ensure_node() {
  if command -v node >/dev/null 2>&1; then
    local node_ver
    node_ver=$(node -v | sed 's/^v//')
    if [ "${node_ver%%.*}" -lt 22 ]; then
      warn "Node.js $node_ver found. Recommended: 22+. Continuing anyway."
    else
      ok "Node.js $node_ver"
    fi
    return 0
  fi

  warn "Node.js not found."
  local pm
  pm=$(detect_pm)
  case "$pm" in
    apt)
      if confirm "Install Node.js 22 using NodeSource"; then
        run sudo_cmd apt-get update
        run sudo_cmd apt-get install -y ca-certificates curl gnupg
        if $DRY_RUN; then
          info "DRY-RUN: curl NodeSource setup script | sudo bash"
        else
          curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
        fi
        run sudo_cmd apt-get install -y nodejs
        return 0
      fi
      ;;
    dnf|yum|pacman|zypper)
      install_packages "$pm" nodejs npm && return 0
      ;;
  esac

  err "Node.js is required. Install Node.js 22+ then rerun the installer."
  exit 1
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

ensure_pm2() {
  ensure_node
  if command -v pm2 >/dev/null 2>&1; then
    ok "PM2 available"
    return 0
  fi
  info "Installing PM2..."
  if $DRY_RUN; then
    info "DRY-RUN: npm install -g pm2"
  else
    npm install -g pm2 2>/dev/null || sudo npm install -g pm2
  fi
}

resolve_project_dir() {
  if [ -f proxy.mjs ] && [ -d src ]; then
    PROJECT_DIR="$(pwd)"
    return 0
  fi

  local script_dir=""
  if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || true)"
  fi
  if [ -n "$script_dir" ] && [ -f "$script_dir/../proxy.mjs" ]; then
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
  if [ -f "$INSTALL_DIR/proxy.mjs" ]; then
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
  echo "  2) PM2             (lightweight, runs on host)"
  echo "  3) Direct node     (foreground, for testing only)"
  echo ""
  local choice
  read -r -p "Choice [1]: " choice
  choice=${choice:-1}
  case "$choice" in
    1) METHOD="docker" ;;
    2) METHOD="pm2" ;;
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
  local url="http://localhost:8318/health"
  if $DRY_RUN; then
    info "DRY-RUN: check $url"
    return 0
  fi
  if command -v curl >/dev/null 2>&1 && curl -sf "$url" >/dev/null 2>&1; then
    ok "Proxy running! http://localhost:8318"
    info "Health: $(curl -s "$url")"
  else
    warn "Health check failed or curl unavailable. Wait a few seconds, then check logs."
  fi
}

run_docker() {
  if ! ensure_docker; then
    warn "Docker unavailable. Falling back to PM2."
    METHOD="pm2"
    run_pm2
    return
  fi
  ensure_env
  info "Starting with Docker Compose..."
  run docker compose up -d --build
  if ! $DRY_RUN; then
    sleep 5
  fi
  health_check
}

run_pm2() {
  ensure_pm2
  ensure_env
  info "Starting with PM2..."
  run pm2 start proxy.mjs --name agentrouter-proxy
  run pm2 save
  info "To auto-start on reboot: pm2 startup (follow printed instructions)"
  if ! $DRY_RUN; then
    sleep 2
  fi
  health_check
  info "Manage: pm2 status | pm2 logs agentrouter-proxy | pm2 restart agentrouter-proxy"
}

run_direct() {
  ensure_node
  ensure_env
  info "Starting proxy in foreground (Ctrl+C to stop)..."
  if $DRY_RUN; then
    info "DRY-RUN: node proxy.mjs"
  else
    node proxy.mjs
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
  docker) run_docker ;;
  pm2) run_pm2 ;;
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
