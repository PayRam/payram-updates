#!/usr/bin/env bash
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

require() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing dependency: $1" >&2
    exit 1
  fi
}

require curl
require sudo

OS="linux"
ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH_RAW" >&2
    exit 1
    ;;
esac

VERSION="${PAYRAM_UPDATER_VERSION:-}"
if [[ -z "$VERSION" ]]; then
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/latest" | \
    grep -m1 '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')"
fi

if [[ -z "$VERSION" ]]; then
  echo "Failed to detect latest version" >&2
  exit 1
fi

ASSET="${BIN_NAME}-${OS}-${ARCH}"
DOWNLOAD_URL="${PAYRAM_UPDATER_DOWNLOAD_URL:-https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${VERSION}/${ASSET}}"

TMP_BIN="/tmp/${BIN_NAME}"

echo "Downloading ${DOWNLOAD_URL}"
rm -f "$TMP_BIN"
curl -fsSL "$DOWNLOAD_URL" -o "$TMP_BIN"
chmod +x "$TMP_BIN"

sudo install -m 0755 "$TMP_BIN" "${INSTALL_DIR}/${BIN_NAME}"

sudo mkdir -p /etc/payram
sudo mkdir -p "$STATE_DIR" "$LOG_DIR" "$BACKUP_DIR"

if [[ ! -f "$ENV_PATH" ]]; then
  echo "Creating ${ENV_PATH}"
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
else
  echo "Using existing ${ENV_PATH}"
fi

if [[ ! -f "$SERVICE_PATH" ]]; then
  echo "Installing systemd service"
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
fi

if sudo test -f "$ROOT_CONFIG" && sudo grep -q '"initialized": true' "$ROOT_CONFIG"; then
  echo "Updater already initialized"
else
  echo "Running updater init (interactive)"
  sudo "${INSTALL_DIR}/${BIN_NAME}" init
fi

sudo systemctl daemon-reload
sudo systemctl enable payram-updater
sudo systemctl restart payram-updater

sudo systemctl status payram-updater --no-pager

echo "Setup complete."
