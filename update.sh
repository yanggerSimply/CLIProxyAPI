#!/usr/bin/env bash
set -euo pipefail

# ─── Configuration ────────────────────────────────────────────────────────────
BACKEND_REPO="https://github.com/yanggerSimply/CLIProxyAPI.git"
WEBUI_REPO="https://github.com/yanggerSimply/Cli-Proxy-API-Management-Center.git"
INSTALL_DIR="$HOME/cliproxyapi"
SRC_DIR="$INSTALL_DIR/.src"
BACKEND_SRC="$SRC_DIR/CLIProxyAPI"
WEBUI_SRC="$SRC_DIR/Cli-Proxy-API-Management-Center"
GO_VERSION="1.24.4"
NODE_VERSION="22"

# ─── Colors ───────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info()    { echo -e "${BLUE}[INFO]${NC} $1"; }
ok()      { echo -e "${GREEN}[OK]${NC} $1"; }
warn()    { echo -e "${YELLOW}[WARN]${NC} $1"; }
fail()    { echo -e "${RED}[FAIL]${NC} $1"; exit 1; }
step()    { echo -e "\n${BLUE}━━━ $1 ━━━${NC}"; }

# ─── Detect architecture ─────────────────────────────────────────────────────
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) fail "Unsupported architecture: $(uname -m)" ;;
  esac
}

detect_os() {
  case "$(uname -s)" in
    Linux)  echo "linux" ;;
    Darwin) echo "darwin" ;;
    *) fail "Unsupported OS: $(uname -s)" ;;
  esac
}

# ─── Ensure Go is installed ──────────────────────────────────────────────────
ensure_go() {
  if command -v go &>/dev/null; then
    info "Go already installed: $(go version)"
    return
  fi

  local os_name arch go_tar
  os_name=$(detect_os)
  arch=$(detect_arch)
  go_tar="go${GO_VERSION}.${os_name}-${arch}.tar.gz"

  step "Installing Go ${GO_VERSION}"
  curl -fsSL "https://go.dev/dl/${go_tar}" -o "/tmp/${go_tar}"
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "/tmp/${go_tar}"
  rm -f "/tmp/${go_tar}"
  export PATH="/usr/local/go/bin:$PATH"
  ok "Go $(go version) installed"
}

# ─── Ensure Node.js is installed (for WebUI build) ───────────────────────────
ensure_node() {
  if command -v node &>/dev/null && command -v npm &>/dev/null; then
    info "Node.js already installed: $(node --version)"
    return
  fi

  step "Installing Node.js ${NODE_VERSION}"
  if command -v apt-get &>/dev/null; then
    curl -fsSL "https://deb.nodesource.com/setup_${NODE_VERSION}.x" | sudo -E bash -
    sudo apt-get install -y nodejs
  elif command -v yum &>/dev/null; then
    curl -fsSL "https://rpm.nodesource.com/setup_${NODE_VERSION}.x" | sudo bash -
    sudo yum install -y nodejs
  elif command -v brew &>/dev/null; then
    brew install node
  else
    fail "Cannot auto-install Node.js. Please install manually."
  fi
  ok "Node.js $(node --version) installed"
}

# ─── Clone or pull a repo ────────────────────────────────────────────────────
sync_repo() {
  local url="$1" dest="$2" name="$3"

  if [[ -d "$dest/.git" ]]; then
    info "Updating ${name}..."
    git -C "$dest" fetch --all --prune
    git -C "$dest" reset --hard origin/main
  else
    info "Cloning ${name}..."
    mkdir -p "$(dirname "$dest")"
    git clone "$url" "$dest"
  fi

  local sha
  sha=$(git -C "$dest" rev-parse --short HEAD)
  ok "${name} synced to ${sha}"
}

# ─── Build backend ───────────────────────────────────────────────────────────
build_backend() {
  step "Building CLIProxyAPI backend"

  local version
  version=$(git -C "$BACKEND_SRC" describe --tags 2>/dev/null || git -C "$BACKEND_SRC" rev-parse --short HEAD)

  cd "$BACKEND_SRC"
  CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.version=${version}" \
    -o "$INSTALL_DIR/cli-proxy-api" \
    ./cmd/server

  chmod +x "$INSTALL_DIR/cli-proxy-api"
  ok "Backend built: ${version}"
}

# ─── Build WebUI ──────────────────────────────────────────────────────────────
build_webui() {
  step "Building Management Center WebUI"

  cd "$WEBUI_SRC"
  npm ci --prefer-offline 2>/dev/null || npm install
  npx vite build

  local static_dir="$INSTALL_DIR/static"
  mkdir -p "$static_dir"
  cp "$WEBUI_SRC/dist/index.html" "$static_dir/management.html"

  ok "WebUI built and deployed to ${static_dir}/management.html"
}

# ─── Copy example config if first install ─────────────────────────────────────
ensure_config() {
  local config="$INSTALL_DIR/config.yaml"
  if [[ -f "$config" ]]; then
    ok "Existing config.yaml preserved"
    return
  fi

  local example="$BACKEND_SRC/config.example.yaml"
  if [[ -f "$example" ]]; then
    cp "$example" "$config"
    ok "Created config.yaml from example — edit it before first run"
  else
    warn "No config.example.yaml found. Create config.yaml manually."
  fi
}

# ─── Point panel-github-repository to your fork ──────────────────────────────
patch_config_panel_repo() {
  local config="$INSTALL_DIR/config.yaml"
  [[ -f "$config" ]] || return

  if grep -q 'panel-github-repository' "$config"; then
    sed -i.bak "s|panel-github-repository:.*|panel-github-repository: \"\"|" "$config"
    rm -f "$config.bak"
    info "Disabled auto-download of official WebUI (using local build)"
  fi
}

# ─── Create / update systemd service ─────────────────────────────────────────
setup_systemd() {
  local os_name
  os_name=$(detect_os)
  if [[ "$os_name" != "linux" ]]; then
    info "Skipping systemd setup on ${os_name}"
    return
  fi

  local systemd_dir="$HOME/.config/systemd/user"
  mkdir -p "$systemd_dir"

  cat > "$systemd_dir/cliproxyapi.service" <<EOF
[Unit]
Description=CLIProxyAPI Service
After=network.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/cli-proxy-api
Restart=always
RestartSec=10
Environment=HOME=$HOME
Environment=MANAGEMENT_STATIC_PATH=$INSTALL_DIR/static

[Install]
WantedBy=default.target
EOF

  systemctl --user daemon-reload
  ok "systemd service updated"
}

# ─── Restart service ─────────────────────────────────────────────────────────
restart_service() {
  local os_name
  os_name=$(detect_os)
  if [[ "$os_name" != "linux" ]]; then
    info "Not Linux — skip service restart. Run manually:"
    info "  cd $INSTALL_DIR && ./cli-proxy-api"
    return
  fi

  if systemctl --user is-active --quiet cliproxyapi.service 2>/dev/null; then
    systemctl --user restart cliproxyapi.service
    sleep 2
    if systemctl --user is-active --quiet cliproxyapi.service; then
      ok "Service restarted successfully"
    else
      warn "Service may not have started. Check: systemctl --user status cliproxyapi.service"
    fi
  else
    info "Service not running. Start with:"
    info "  systemctl --user enable --now cliproxyapi.service"
  fi
}

# ─── Main ─────────────────────────────────────────────────────────────────────
main() {
  echo -e "${GREEN}╔══════════════════════════════════════════════╗${NC}"
  echo -e "${GREEN}║   CLIProxyAPI Fork — Build & Deploy Script   ║${NC}"
  echo -e "${GREEN}╚══════════════════════════════════════════════╝${NC}"

  mkdir -p "$INSTALL_DIR" "$SRC_DIR"

  step "Checking dependencies"
  ensure_go
  ensure_node

  step "Syncing repositories"
  sync_repo "$BACKEND_REPO"  "$BACKEND_SRC"  "CLIProxyAPI"
  sync_repo "$WEBUI_REPO"    "$WEBUI_SRC"    "Management Center"

  build_backend
  build_webui
  ensure_config
  patch_config_panel_repo
  setup_systemd
  restart_service

  echo ""
  echo -e "${GREEN}╔══════════════════════════════════════════════╗${NC}"
  echo -e "${GREEN}║              All done!                       ║${NC}"
  echo -e "${GREEN}╚══════════════════════════════════════════════╝${NC}"
  echo ""
  info "Install dir:  $INSTALL_DIR"
  info "Binary:       $INSTALL_DIR/cli-proxy-api"
  info "WebUI:        $INSTALL_DIR/static/management.html"
  info "Config:       $INSTALL_DIR/config.yaml"
  info "Source:       $SRC_DIR"
  echo ""
  info "Next time you push changes to either fork, just re-run:"
  echo -e "  ${YELLOW}bash ~/cliproxyapi/.src/update.sh${NC}"
  echo -e "  or"
  echo -e "  ${YELLOW}curl -fsSL https://raw.githubusercontent.com/yanggerSimply/CLIProxyAPI/main/update.sh | bash${NC}"
}

main "$@"
