#!/usr/bin/env bash
set -euo pipefail

# ──────────────────────────────────────────────────────────
# AgentRouter Spoof Proxy — Linux One-Line Installer
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash
#
# Or clone manually:
#   git clone https://github.com/trefeon/agentrouter-spoof-proxy.git
#   cd agentrouter-spoof-proxy && bash scripts/install.sh
# ──────────────────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
err()   { echo -e "${RED}[ERROR]${NC} $*"; }

# ── Detect project root ──
if [ -f proxy.mjs ]; then
  PROJECT_DIR="$(pwd)"
else
  PROJECT_DIR="$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)"
  [ -f "$PROJECT_DIR/proxy.mjs" ] || { err "Cannot find proxy.mjs. Run from agentrouter-spoof-proxy directory."; exit 1; }
fi
cd "$PROJECT_DIR"

echo ""
info "AgentRouter Spoof Proxy Installer"
info "Project dir: $PROJECT_DIR"
echo ""

# ── Check Node.js ──
if command -v node &>/dev/null; then
  NODE_VER=$(node -v | sed 's/v//')
  [ "${NODE_VER%%.*}" -ge 22 ] || {
    warn "Node.js $NODE_VER found. Recommended: 22+. Continuing anyway..."
  }
  ok "Node.js $NODE_VER"
else
  err "Node.js not found. Install it first:"
  echo "  Ubuntu/Debian: sudo apt install nodejs"
  echo "  Arch:          sudo pacman -S nodejs"
  echo "  Or use nvm:    curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.1/install.sh | bash"
  exit 1
fi

# ── Check Docker (optional) ──
USE_DOCKER=false
if command -v docker &>/dev/null && docker info &>/dev/null 2>&1; then
  USE_DOCKER=true
fi

# ── Pick install method ──
echo ""
echo "How do you want to run the proxy?"
echo "  1) Docker          (recommended — auto-restart, isolated)"
echo "  2) PM2             (lightweight, runs on host)"
echo "  3) Direct node     (foreground, for testing only)"
echo ""
read -p "Choice [1]: " CHOICE
CHOICE=${CHOICE:-1}
echo ""

case "$CHOICE" in
  1)
    if ! $USE_DOCKER; then
      err "Docker not available. Choose option 2 or 3."
      exit 1
    fi
    info "Starting with Docker Compose..."
    [ -f .env ] || cp .env.example .env
    docker compose up -d --build
    sleep 3
    info "Waiting for WAF warmup..."
    sleep 2
    if curl -sf http://localhost:8318/health >/dev/null 2>&1; then
      ok "Proxy running! http://localhost:8318"
      info "Health: $(curl -s http://localhost:8318/health)"
    else
      warn "Proxy started but health check failed. Check: docker logs agentrouter-proxy"
    fi
    ;;

  2)
    info "Installing PM2..."
    npm install -g pm2 2>/dev/null || sudo npm install -g pm2
    [ -f .env ] || cp .env.example .env
    pm2 start proxy.mjs --name agentrouter-proxy
    pm2 save
    echo ""
    info "To auto-start on reboot: pm2 startup (follow printed instructions)"
    sleep 2
    if curl -sf http://localhost:8318/health >/dev/null 2>&1; then
      ok "Proxy running! http://localhost:8318"
    else
      warn "Proxy started but WAF may still be warming up. Wait a few seconds."
    fi
    echo ""
    info "Manage: pm2 status | pm2 logs agentrouter-proxy | pm2 restart agentrouter-proxy"
    ;;

  3)
    [ -f .env ] || cp .env.example .env
    info "Starting proxy in foreground (Ctrl+C to stop)..."
    echo ""
    node proxy.mjs
    ;;

  *)
    err "Invalid choice. Run again."
    exit 1
    ;;
esac

echo ""
ok "Done! Proxy is at http://localhost:8318"
echo ""
info "Next steps:"
echo "  - 9Router:      Add custom OpenAI Compatible provider → Base URL: http://localhost:8318/v1 → Import models from /models"
echo "  - Direct test:  curl http://localhost:8318/v1/messages -H 'Authorization: Bearer YOUR_KEY' -H 'Content-Type: application/json' -d '{\"model\":\"claude-opus-4-8\",\"max_tokens\":10,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
echo "  - Docs:         https://github.com/trefeon/agentrouter-spoof-proxy/blob/main/docs/panduan-9router.md"
echo ""
