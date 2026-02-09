#!/usr/bin/env bash
# PayRam Updater Setup Script
#
# Usage:
#   Interactive mode:     bash <(curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh)
#   Non-interactive:      curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | bash
#   Force reinstall:      curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | FORCE_REINSTALL=true bash
#   Specific version:     curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | PAYRAM_UPDATER_VERSION=v0.1.0 bash
#
set -euo pipefail

REPO_OWNER="PayRam"
REPO_NAME="payram-updates"
BIN_NAME="payram-updater"
INSTALL_DIR="/usr/local/bin"
SERVICE_PATH="/etc/systemd/system/payram-updater.service"
ENV_PATH="/etc/payram/updater.env"
STATE_DIR="/var/lib/payram-updater"
LOG_DIR="/var/log/payram-updater"
BACKUP_DIR="/var/lib/payram/backups"
ROOT_CONFIG="/root/.payram-updates.config"

# Check if running interactively
if [[ -t 0 ]]; then
  INTERACTIVE=true
else
  INTERACTIVE=false
fi

# Force reinstall mode (set FORCE_REINSTALL=true to skip prompts)
FORCE_REINSTALL="${FORCE_REINSTALL:-false}"

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
}

require() {
  if ! command -v "$1" >/dev/null 2>&1; then
    log "ERROR: Missing dependency: $1"
    exit 1
  fi
}

log "Starting PayRam Updater setup..."

log "Checking dependencies..."
require curl
require sudo

log "Detecting system architecture..."
OS="linux"
ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    log "ERROR: Unsupported architecture: $ARCH_RAW"
    exit 1
    ;;
esac
log "Detected: ${OS}-${ARCH}"

log "Fetching latest version..."
VERSION="${PAYRAM_UPDATER_VERSION:-}"
if [[ -z "$VERSION" ]]; then
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/latest" 2>&1 | \
    grep -m1 '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/' || true)"
fi

if [[ -z "$VERSION" ]]; then
  log "ERROR: Failed to detect latest version. Set PAYRAM_UPDATER_VERSION env var to specify manually."
  exit 1
fi
log "Latest version: $VERSION"

ASSET="${BIN_NAME}-${OS}-${ARCH}"
DOWNLOAD_URL="${PAYRAM_UPDATER_DOWNLOAD_URL:-https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${VERSION}/${ASSET}}"

TMP_BIN="/tmp/${BIN_NAME}"

log "Downloading ${DOWNLOAD_URL}..."
rm -f "$TMP_BIN"
curl -fsSL "$DOWNLOAD_URL" -o "$TMP_BIN"
chmod +x "$TMP_BIN"
log "Download complete"

if [[ -f "${INSTALL_DIR}/${BIN_NAME}" ]]; then
  CURRENT_VERSION=$("${INSTALL_DIR}/${BIN_NAME}" version 2>/dev/null || echo "unknown")
  log "Existing binary found (version: $CURRENT_VERSION)"
  
  if [[ "$FORCE_REINSTALL" == "true" ]]; then
    log "Force reinstall mode - overriding existing binary"
    OVERRIDE_BIN="y"
  elif [[ "$INTERACTIVE" == "true" ]]; then
    read -p "Override existing binary? [Y/n]: " OVERRIDE_BIN
  else
    log "Non-interactive mode - overriding existing binary"
    OVERRIDE_BIN="y"
  fi
  
  if [[ ! "$OVERRIDE_BIN" =~ ^[Nn] ]]; then
    log "Installing binary to ${INSTALL_DIR}/${BIN_NAME}..."
    sudo install -m 0755 "$TMP_BIN" "${INSTALL_DIR}/${BIN_NAME}"
    log "Binary installed successfully"
  else
    log "Keeping existing binary"
  fi
else
  log "Installing binary to ${INSTALL_DIR}/${BIN_NAME}..."
  sudo install -m 0755 "$TMP_BIN" "${INSTALL_DIR}/${BIN_NAME}"
  log "Binary installed successfully"
fi

log "Creating required directories..."
sudo mkdir -p /etc/payram
sudo mkdir -p "$STATE_DIR" "$LOG_DIR" "$BACKUP_DIR"
log "Directories created"

log "Directories created"

if [[ -f "$ENV_PATH" ]]; then
  log "Environment file already exists: $ENV_PATH"
  
  if [[ "$FORCE_REINSTALL" == "true" ]]; then
    log "Force reinstall mode - overriding environment file"
    OVERRIDE_ENV="y"
  elif [[ "$INTERACTIVE" == "true" ]]; then
    read -p "Override existing environment file? [y/N]: " OVERRIDE_ENV
  else
    log "Non-interactive mode - keeping existing environment file"
    OVERRIDE_ENV="n"
  fi
  
  if [[ "$OVERRIDE_ENV" =~ ^[Yy] ]]; then
    log "Creating new environment file at $ENV_PATH..."
    sudo tee "$ENV_PATH" >/dev/null <<EOF
# Payram Updater configuration
POLICY_URL=${POLICY_URL:-https://raw.githubusercontent.com/PayRam/payram-policies/main/upgrade-policy.json}
RUNTIME_MANIFEST_URL=${RUNTIME_MANIFEST_URL:-https://raw.githubusercontent.com/PayRam/payram-updates/main/runtime-manifest.json}
FETCH_TIMEOUT_SECONDS=${FETCH_TIMEOUT_SECONDS:-10}
STATE_DIR=${STATE_DIR}
LOG_DIR=${LOG_DIR}
BACKUP_DIR=${BACKUP_DIR}
EXECUTION_MODE=${EXECUTION_MODE:-execute}
DOCKER_BIN=${DOCKER_BIN:-docker}
EOF
    log "Environment file created"
  else
    log "Using existing environment file"
  fi
else
  log "Creating environment file at $ENV_PATH..."
  sudo tee "$ENV_PATH" >/dev/null <<EOF
# Payram Updater configuration
POLICY_URL=${POLICY_URL:-https://raw.githubusercontent.com/PayRam/payram-policies/main/upgrade-policy.json}
RUNTIME_MANIFEST_URL=${RUNTIME_MANIFEST_URL:-https://raw.githubusercontent.com/PayRam/payram-updates/main/runtime-manifest.json}
FETCH_TIMEOUT_SECONDS=${FETCH_TIMEOUT_SECONDS:-10}
STATE_DIR=${STATE_DIR}
LOG_DIR=${LOG_DIR}
BACKUP_DIR=${BACKUP_DIR}
EXECUTION_MODE=${EXECUTION_MODE:-execute}
DOCKER_BIN=${DOCKER_BIN:-docker}
EOF
  log "Environment file created"
fi

if [[ -f "$SERVICE_PATH" ]]; then
  log "Systemd service already exists: $SERVICE_PATH"
  
  if [[ "$FORCE_REINSTALL" == "true" ]]; then
    log "Force reinstall mode - overriding service file"
    OVERRIDE_SERVICE="y"
  elif [[ "$INTERACTIVE" == "true" ]]; then
    read -p "Override existing service file? [y/N]: " OVERRIDE_SERVICE
  else
    log "Non-interactive mode - keeping existing service file"
    OVERRIDE_SERVICE="n"
  fi
  
  if [[ "$OVERRIDE_SERVICE" =~ ^[Yy] ]]; then
    log "Installing systemd service at $SERVICE_PATH..."
    sudo tee "$SERVICE_PATH" >/dev/null <<'EOF'
[Unit]
Description=Payram Updater Service
Documentation=https://github.com/payram/payram-updater
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
EnvironmentFile=/etc/payram/updater.env
ExecStart=/usr/local/bin/payram-updater serve
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=payram-updater

# Security hardening
NoNewPrivileges=false
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/var/lib/payram-updater /var/log/payram-updater /var/lib/payram /root

[Install]
WantedBy=multi-user.target
EOF
    log "Systemd service installed"
  else
    log "Using existing service file"
  fi
else
  log "Installing systemd service at $SERVICE_PATH..."
  sudo tee "$SERVICE_PATH" >/dev/null <<'EOF'
[Unit]
Description=Payram Updater Service
Documentation=https://github.com/payram/payram-updater
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
EnvironmentFile=/etc/payram/updater.env
ExecStart=/usr/local/bin/payram-updater serve
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=payram-updater

# Security hardening
NoNewPrivileges=false
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/var/lib/payram-updater /var/log/payram-updater /var/lib/payram /root

[Install]
WantedBy=multi-user.target
EOF
  log "Systemd service installed"
fi

log "Checking initialization status..."
if sudo test -f "$ROOT_CONFIG" && sudo grep -q '"initialized": true' "$ROOT_CONFIG"; then
  log "Updater already initialized"
  
  if [[ "$FORCE_REINSTALL" == "true" ]]; then
    log "Force reinstall mode - skipping re-initialization"
    REINIT="n"
  elif [[ "$INTERACTIVE" == "true" ]]; then
    read -p "Re-run initialization? [y/N]: " REINIT
  else
    log "Non-interactive mode - skipping re-initialization"
    REINIT="n"
  fi
  
  if [[ "$REINIT" =~ ^[Yy] ]]; then
    log "Running updater init (interactive)..."
    sudo "${INSTALL_DIR}/${BIN_NAME}" init
    log "Initialization complete"
  else
    log "Skipping initialization"
  fi
else
  log "Running updater init (interactive)..."
  sudo "${INSTALL_DIR}/${BIN_NAME}" init
  log "Initialization complete"
fi

log "Reloading systemd daemon..."
sudo systemctl daemon-reload

log "Enabling payram-updater service..."
sudo systemctl enable payram-updater

log "Restarting payram-updater service..."
sudo systemctl restart payram-updater

log "Checking service status..."
sudo systemctl status payram-updater --no-pager

log "Setup complete!"
