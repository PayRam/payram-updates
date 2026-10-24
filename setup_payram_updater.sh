#!/usr/bin/env bash
# PayRam Updater Setup Script
#
# Installs or upgrades the PayRam Updater, the small service that upgrades the
# PayRam container when you click Update in the dashboard. Safe to re-run:
#   - the new binary is checksum-verified and test-run before it replaces the
#     current one, and the current one is kept as payram-updater.previous;
#   - if the new updater does not answer after the restart, the previous one is
#     put back and the script exits non-zero;
#   - if a PayRam update is in progress, it waits for it to finish before
#     restarting anything (and gives up without touching anything after 30 min);
#   - /etc/payram/updater.env is kept: settings it already has are never changed,
#     missing ones are added with their defaults (a backup is kept first);
#   - the service definition is always refreshed (a backup of the old one is kept).
#
# Usage (run as root, or pass the variables through sudo as shown):
#   Install / upgrade:    curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo bash
#   Interactive mode:     bash <(curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh)
#   Specific version:     curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo PAYRAM_UPDATER_VERSION=v0.1.0 bash
#   Pin script + binary:  curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/<tag>/setup_payram_updater.sh | sudo PAYRAM_UPDATER_VERSION=<tag> PAYRAM_UPDATER_SHA256=<sha256> bash
#   Force reinstall:      curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo FORCE_REINSTALL=true bash
#   Custom port:          curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo UPDATER_PORT=3000 bash
#   Debug mode:           curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo DEBUG_VERSION_MODE=true bash
#   Quiet mode:           curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo QUIET=true bash
#   Without auto-start:   curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo ENABLE_SERVICE=false bash
#   Uninstall:            curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo bash -s -- --uninstall
#   Uninstall (no prompt): curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo bash -s -- --uninstall --yes
#
# Environment Variables (optional). Settings marked [env file] are written to
# /etc/payram/updater.env on a new install, or added to it when missing; a
# value the file already has is never changed (edit the file to change it).
#   UPDATER_PORT              - [env file] API port (default: 2567)
#   DEBUG_VERSION_MODE        - [env file] Enable debug mode (default: false)
#   POLICY_URL                - [env file] Upgrade policy URL
#   RUNTIME_MANIFEST_URL      - [env file] Runtime manifest URL
#   FETCH_TIMEOUT_SECONDS     - [env file] Fetch timeout (default: 10)
#   EXECUTION_MODE            - [env file] Execution mode: dry-run or execute (default: execute)
#   DOCKER_BIN                - [env file] Docker binary path (default: docker)
#   HEALTH_CHECK_TIMEOUT_SECONDS - [env file] How long an upgrade waits for PayRam to come up healthy (default: 600)
#   PAYRAM_UPDATER_VERSION    - Specific version to install (default: latest)
#   PAYRAM_UPDATER_CHECKSUM_URL - Override checksum URL for the binary (default: ${DOWNLOAD_URL}.sha256)
#   PAYRAM_UPDATER_SHA256      - Provide checksum directly (skips checksum download)
#   FORCE_REINSTALL           - Skip all prompts and reinstall the binary (default: false)
#   QUIET                     - Suppress progress output; errors are still printed (default: false)
#   ENABLE_SERVICE            - Enable and (re)start the service after install (default: true)
#   SETUP_BUSY_WAIT_SECONDS   - How long to wait for an update in progress (default: 1800)
#   SETUP_HEALTH_WAIT_SECONDS - How long the restarted updater has to answer /health (default: 60)
#
set -euo pipefail

REPO_OWNER="PayRam"
REPO_NAME="payram-updates"
BIN_NAME="payram-updater"
SERVICE_NAME="payram-updater"
INSTALL_DIR="/usr/local/bin"
# SERVICE_PATH is set by detect_os
ENV_PATH="/etc/payram/updater.env"
STATE_DIR="/var/lib/payram-updater"
BACKUP_DIR="/var/lib/payram/backups"
INIT_CONFIG="$STATE_DIR/updater-config.json"
# Copies of the env file and service files as they were before this script changed them.
INSTALLER_BACKUP_DIR="/etc/payram/installer-backups"
LAUNCHER_PATH="/usr/local/bin/payram-updater-launcher"

DEFAULT_POLICY_URL="https://raw.githubusercontent.com/PayRam/payram-scripts/refs/heads/main/updater-configs/upgrade-policy.json"
DEFAULT_RUNTIME_MANIFEST_URL="https://raw.githubusercontent.com/PayRam/payram-scripts/refs/heads/main/updater-configs/runtime-manifest.json"

# Job states in which the updater is in the middle of an update (jobs.Job.IsActive).
ACTIVE_JOB_STATES="POLICY_FETCHING MANIFEST_FETCHING BACKING_UP EXECUTING VERIFYING"
# A daemon that heartbeats saves an active job at least every 30s, so a job
# that has not been saved for this long is not being worked on.
HEARTBEAT_STALE_SECONDS=300

SETUP_BUSY_WAIT_SECONDS="${SETUP_BUSY_WAIT_SECONDS:-1800}"
SETUP_BUSY_POLL_SECONDS="${SETUP_BUSY_POLL_SECONDS:-10}"
SETUP_HEALTH_WAIT_SECONDS="${SETUP_HEALTH_WAIT_SECONDS:-60}"
SETUP_HEALTH_POLL_SECONDS="${SETUP_HEALTH_POLL_SECONDS:-2}"

# Check if running interactively
if [[ -t 0 ]]; then
  INTERACTIVE=true
else
  INTERACTIVE=false
fi

# Force reinstall mode (set FORCE_REINSTALL=true to skip prompts)
FORCE_REINSTALL="${FORCE_REINSTALL:-false}"
QUIET="${QUIET:-false}"
ENABLE_SERVICE="${ENABLE_SERVICE:-true}"

UNINSTALL_MODE=false
UNINSTALL_YES=false
REMOVE_BACKUPS=false

# Set while running
WORK_DIR=""
BINARY_REPLACED=false
PREVIOUS_SAVED=false
CURRENT_VERSION="none"
NEW_VERSION=""
UPDATER_HTTP_PORT=2567
DAEMON_HEARTBEATS=false
LAST_BUSY_STATE=""
STALE_REASON=""

# Commands that need root run through $SUDO: sudo for a user that is not
# root, nothing when the script already runs as root (many servers are run as
# root and have no sudo installed; setup_payram.sh runs this script as root).
if [[ "$(id -u)" -eq 0 ]]; then
  SUDO=""
else
  SUDO="sudo"
fi

log() {
  if [[ "$QUIET" != "true" ]]; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
  fi
}

# Errors are printed even in quiet mode, on stderr.
log_error() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] ERROR: $*" >&2
}

require() {
  if ! command -v "$1" >/dev/null 2>&1; then
    log_error "Missing dependency: $1"
    exit 1
  fi
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --uninstall)
        UNINSTALL_MODE=true
        ;;
      --yes|-y)
        UNINSTALL_YES=true
        ;;
      --remove-backups)
        REMOVE_BACKUPS=true
        ;;
    esac
    shift
  done
}

detect_os() {
  local os_raw
  os_raw="$(uname -s)"
  case "$os_raw" in
    Linux)
      OS="linux"
      SERVICE_PATH="/etc/systemd/system/payram-updater.service"
      ;;
    Darwin)
      OS="darwin"
      SERVICE_PATH="/Library/LaunchDaemons/com.payram.updater.plist"
      ;;
    *)
      echo "ERROR: Unsupported operating system: $os_raw"
      echo "PayRam Updater supports Linux and macOS."
      exit 1
      ;;
  esac
}

detect_arch() {
  local arch_raw
  arch_raw="$(uname -m)"
  case "$arch_raw" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)
      log_error "Unsupported architecture: $arch_raw"
      exit 1
      ;;
  esac
}

set_binary_paths() {
  INSTALLED_BIN="${INSTALL_DIR}/${BIN_NAME}"
  # The binary that was installed before this run, kept so it can be put back.
  PREVIOUS_BIN="${INSTALL_DIR}/${BIN_NAME}.previous"
  # The verified download, copied next to the installed binary so the final
  # replacement is a single rename (and so it runs even when /tmp is noexec).
  STAGED_BIN="${INSTALL_DIR}/.${BIN_NAME}.staged"
}

cleanup_work_files() {
  if [[ -n "$WORK_DIR" && -d "$WORK_DIR" ]]; then
    rm -rf "$WORK_DIR"
  fi
  if [[ -n "${STAGED_BIN:-}" ]] && [[ -e "$STAGED_BIN" ]]; then
    $SUDO rm -f "$STAGED_BIN" 2>/dev/null || true
  fi
}

# ----------------------------------------------------------------------------
# Env file helpers
# ----------------------------------------------------------------------------

# env_file_value KEY: the value the updater would read for KEY from the env
# file (the last assignment wins, as in systemd), without surrounding quotes.
# Prints nothing when the file or the key is missing. (awk uses [ \t], not
# [[:space:]]: mawk 1.3.3 on older Ubuntu/Debian has no character classes.)
env_file_value() {
  local key="$1"
  if ! $SUDO test -f "$ENV_PATH"; then
    return 0
  fi
  $SUDO cat "$ENV_PATH" 2>/dev/null | awk -v k="$key" '
    /^[ \t]*#/ { next }
    {
      line = $0
      sub(/^[ \t]+/, "", line)
      i = index(line, "=")
      if (i == 0) next
      name = substr(line, 1, i - 1)
      sub(/[ \t]+$/, "", name)
      if (name != k) next
      v = substr(line, i + 1)
      sub(/^[ \t]+/, "", v)
      sub(/[ \t\r]+$/, "", v)
      if (length(v) >= 2 && substr(v, 1, 1) == "\"" && substr(v, length(v), 1) == "\"") v = substr(v, 2, length(v) - 2)
      val = v
      found = 1
    }
    END { if (found) print val }' || true
}

# env_file_has_key KEY: true when the env file assigns KEY (comments don't count).
env_file_has_key() {
  local key="$1"
  if ! $SUDO test -f "$ENV_PATH"; then
    return 1
  fi
  $SUDO cat "$ENV_PATH" 2>/dev/null | awk -v k="$key" '
    /^[ \t]*#/ { next }
    {
      line = $0
      sub(/^[ \t]+/, "", line)
      i = index(line, "=")
      if (i == 0) next
      name = substr(line, 1, i - 1)
      sub(/[ \t]+$/, "", name)
      if (name == k) found = 1
    }
    END { exit found ? 0 : 1 }'
}

# The settings a new env file gets, one KEY=VALUE per line. Missing keys are
# added to an existing file from this same list.
env_template() {
  cat <<EOF
POLICY_URL=${POLICY_URL:-$DEFAULT_POLICY_URL}
RUNTIME_MANIFEST_URL=${RUNTIME_MANIFEST_URL:-$DEFAULT_RUNTIME_MANIFEST_URL}
FETCH_TIMEOUT_SECONDS=${FETCH_TIMEOUT_SECONDS:-10}
STATE_DIR=${STATE_DIR}
BACKUP_DIR=${BACKUP_DIR}
EXECUTION_MODE=${EXECUTION_MODE:-execute}
DOCKER_BIN=${DOCKER_BIN:-docker}
SUPERVISOR_EXCLUDE=${SUPERVISOR_EXCLUDE:-postgres,postgresql}
SUPERVISOR_INCLUDE=${SUPERVISOR_INCLUDE:-}
UPDATER_PORT=${UPDATER_PORT:-2567}
DEBUG_VERSION_MODE=${DEBUG_VERSION_MODE:-false}
HEALTH_CHECK_TIMEOUT_SECONDS=${HEALTH_CHECK_TIMEOUT_SECONDS:-600}
EOF
}

# backup_file PATH MODE: copy PATH into INSTALLER_BACKUP_DIR with a timestamp.
backup_file() {
  local src="$1" mode="$2" dest
  $SUDO mkdir -p "$INSTALLER_BACKUP_DIR"
  $SUDO chmod 0700 "$INSTALLER_BACKUP_DIR"
  dest="${INSTALLER_BACKUP_DIR}/$(basename "$src").$(date '+%Y%m%d-%H%M%S')"
  $SUDO cp "$src" "$dest"
  $SUDO chmod "$mode" "$dest"
  log "Backed up $src to $dest"
}

write_env_file() {
  if ! $SUDO test -f "$ENV_PATH"; then
    log "Creating environment file at $ENV_PATH..."
    {
      echo "# Payram Updater configuration"
      env_template
    } | $SUDO tee "$ENV_PATH" >/dev/null
    log "Environment file created"
    return 0
  fi

  log "Environment file already exists: $ENV_PATH (keeping every setting in it, adding only missing ones)"
  local line key given current missing_count=0
  local missing_file="$WORK_DIR/env-missing"
  : >"$missing_file"
  while IFS= read -r line; do
    key="${line%%=*}"
    if env_file_has_key "$key"; then
      # Tell the merchant when a value they passed is not the one in effect.
      case "$key" in
        STATE_DIR|BACKUP_DIR) ;;
        *)
          given="${!key:-}"
          if [[ -n "$given" ]]; then
            current="$(env_file_value "$key")"
            if [[ "$current" != "$given" ]]; then
              log "Note: ${key}=${given} was given, but $ENV_PATH already sets ${key}=${current}; keeping the file's value (edit the file to change it)"
            fi
          fi
          ;;
      esac
    else
      echo "$line" >>"$missing_file"
      missing_count=$((missing_count + 1))
    fi
  done < <(env_template)

  if [[ "$missing_count" -eq 0 ]]; then
    log "Environment file already has every setting"
    return 0
  fi

  backup_file "$ENV_PATH" 0600
  {
    # Make sure the first added line does not join the file's last line.
    if [[ -n "$($SUDO tail -c 1 "$ENV_PATH" 2>/dev/null)" ]]; then
      echo ""
    fi
    echo "# Added by setup_payram_updater.sh on $(date '+%Y-%m-%d %H:%M:%S'): settings this file did not have yet"
    cat "$missing_file"
  } | $SUDO tee -a "$ENV_PATH" >/dev/null
  while IFS= read -r line; do
    log "  added ${line}"
  done <"$missing_file"
  log "Environment file updated ($missing_count setting(s) added)"
}

# The port the updater service listens on: what its env file says, else the
# updater's default. (UPDATER_PORT given to this script only reaches the
# service through the env file.)
effective_updater_port() {
  local port
  port="$(env_file_value UPDATER_PORT)"
  # The updater falls back to 2567 when the value is missing or not a number.
  if [[ ! "$port" =~ ^[0-9]+$ ]]; then
    port=2567
  fi
  echo "$port"
}

# ----------------------------------------------------------------------------
# Talking to the running updater (it listens on 127.0.0.1 only)
# ----------------------------------------------------------------------------

# local_api_get PATH [MAX_SECONDS]: GET http://127.0.0.1:<port>PATH, body on stdout.
local_api_get() {
  curl --noproxy '*' -fsS --max-time "${2:-5}" "http://127.0.0.1:${UPDATER_HTTP_PORT}$1" 2>/dev/null
}

updater_answers_health() {
  local body
  body="$(local_api_get /health 3)" || return 1
  echo "$body" | grep -q '"status"[[:space:]]*:[[:space:]]*"ok"'
}

# json_string_field KEY: first "KEY":"value" in the JSON on stdin.
json_string_field() {
  grep -o "\"$1\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" | head -n 1 | sed -E 's/.*:[[:space:]]*"([^"]*)"$/\1/' || true
}

is_active_job_state() {
  local state="$1" active
  for active in $ACTIVE_JOB_STATES; do
    if [[ "$state" == "$active" ]]; then
      return 0
    fi
  done
  return 1
}

# iso_to_epoch TIMESTAMP: seconds since the epoch for an RFC 3339 timestamp
# such as 2026-10-23T08:20:00.123456789Z; prints nothing if it can't parse it.
iso_to_epoch() {
  local ts="$1" base
  if [[ "$ts" == *Z ]]; then
    base="${ts%Z}"
    base="${base%%.*}"
    date -u -d "${base/T/ }" +%s 2>/dev/null ||
      date -j -u -f '%Y-%m-%dT%H:%M:%S' "$base" +%s 2>/dev/null ||
      true
  else
    date -d "$ts" +%s 2>/dev/null || true
  fi
}

# PID of the running updater daemon, or nothing.
daemon_pid() {
  local pid=""
  if [[ "$OS" == "linux" ]] && command -v systemctl >/dev/null 2>&1; then
    pid="$(systemctl show -p MainPID "$SERVICE_NAME" 2>/dev/null | cut -d= -f2 || true)"
  fi
  if [[ -z "$pid" || "$pid" == "0" ]] && command -v pgrep >/dev/null 2>&1; then
    pid="$(pgrep -o -f "${INSTALLED_BIN}( serve)?\$" 2>/dev/null || true)"
  fi
  if [[ "$pid" =~ ^[0-9]+$ && "$pid" != "0" ]]; then
    echo "$pid"
  fi
}

# Seconds since process PID started ("ps -o etime" is [[dd-]hh:]mm:ss everywhere).
process_age_seconds() {
  ps -o etime= -p "$1" 2>/dev/null | awk '
    NF {
      s = $1; d = 0
      if (index(s, "-") > 0) { split(s, a, "-"); d = a[1]; s = a[2] }
      n = split(s, b, ":"); t = 0
      for (i = 1; i <= n; i++) t = t * 60 + b[i]
      print d * 86400 + t
    }' || true
}

# job_is_stale STATUS_JSON: true when the active job in STATUS_JSON cannot be
# running: it was last saved before the running updater process started (an
# updater that stopped mid-update left it behind), or, for an updater that
# saves active jobs every 30s, it has not been saved for 5 minutes.
# Sets STALE_REASON.
job_is_stale() {
  local updated updated_epoch now pid age
  updated="$(echo "$1" | json_string_field updatedAt)"
  updated_epoch="$(iso_to_epoch "$updated")"
  if [[ ! "$updated_epoch" =~ ^[0-9]+$ ]]; then
    return 1
  fi
  now="$(date +%s)"
  if [[ "$DAEMON_HEARTBEATS" == "true" ]] && (( now - updated_epoch > HEARTBEAT_STALE_SECONDS )); then
    STALE_REASON="it has not made progress since ${updated}"
    return 0
  fi
  pid="$(daemon_pid)"
  if [[ -n "$pid" ]]; then
    age="$(process_age_seconds "$pid")"
    # 3s of slack for whole-second rounding on both sides.
    if [[ "$age" =~ ^[0-9]+$ ]] && (( updated_epoch < now - age - 3 )); then
      STALE_REASON="it was last saved at ${updated}, before the running updater started"
      return 0
    fi
  fi
  return 1
}

# wait_until_updater_idle: returns once no update is in progress (or the
# updater does not answer); returns 1 if one is still running after
# SETUP_BUSY_WAIT_SECONDS. Restarting the updater in the middle of an update
# could leave PayRam stopped, so nothing may restart it while this says busy.
wait_until_updater_idle() {
  local started now elapsed status state target
  started="$(date +%s)"
  while true; do
    if ! status="$(local_api_get /upgrade/status 5)"; then
      log "No updater answering on 127.0.0.1:${UPDATER_HTTP_PORT} (not installed or not running); no update to wait for"
      return 0
    fi
    state="$(echo "$status" | json_string_field state)"
    if ! is_active_job_state "$state"; then
      if [[ -n "$state" ]]; then
        log "No update in progress (updater state: ${state})"
      fi
      return 0
    fi
    if job_is_stale "$status"; then
      log "The updater reports an update in state ${state}, but ${STALE_REASON}: no update is actually running."
      log "Continuing; the updater checks and repairs a half-finished update when it starts."
      return 0
    fi

    LAST_BUSY_STATE="$state"
    now="$(date +%s)"
    elapsed=$((now - started))
    if (( elapsed >= SETUP_BUSY_WAIT_SECONDS )); then
      return 1
    fi
    target="$(echo "$status" | json_string_field resolvedTarget)"
    if [[ -z "$target" ]]; then
      target="$(echo "$status" | json_string_field requestedTarget)"
    fi
    log "A PayRam update is in progress (state: ${state}${target:+, to ${target}}); waiting for it to finish before touching the updater ($((elapsed / 60))m$((elapsed % 60))s of $((SETUP_BUSY_WAIT_SECONDS / 60))m)..."
    sleep "$SETUP_BUSY_POLL_SECONDS"
  done
}

# abort_busy WHAT_WAS_DONE: give up because an update is still running.
abort_busy() {
  log_error "A PayRam update has been running for more than $((SETUP_BUSY_WAIT_SECONDS / 60)) minutes (state: ${LAST_BUSY_STATE})."
  log_error "$1"
  log_error "Check it with:  payram-updater status   and   payram-updater logs"
  log_error "Run this script again once the update has finished."
  exit 1
}

wait_for_updater_health() {
  local waited=0
  while (( waited < SETUP_HEALTH_WAIT_SECONDS )); do
    if updater_answers_health; then
      return 0
    fi
    sleep "$SETUP_HEALTH_POLL_SECONDS"
    waited=$((waited + SETUP_HEALTH_POLL_SECONDS))
  done
  updater_answers_health
}

# Wait (up to 30s) until the updater stops answering, after launchd was told to stop it.
wait_for_updater_down() {
  local waited=0
  while (( waited < 30 )) && updater_answers_health; do
    sleep 1
    waited=$((waited + 1))
  done
}

restart_updater_service() {
  if [[ "$OS" == "linux" ]]; then
    $SUDO systemctl restart "$SERVICE_NAME"
  else
    $SUDO launchctl unload "$SERVICE_PATH" 2>/dev/null || true
    wait_for_updater_down
    $SUDO launchctl load "$SERVICE_PATH"
  fi
}

service_logs_hint() {
  if [[ "$OS" == "linux" ]]; then
    echo "sudo journalctl -u ${SERVICE_NAME} -n 100 --no-pager"
  else
    echo "tail -n 100 /var/log/payram-updater.log /var/log/payram-updater.error.log"
  fi
}

# ----------------------------------------------------------------------------
# Uninstall
# ----------------------------------------------------------------------------

do_uninstall() {
  log "Starting PayRam Updater uninstall..."
  set_binary_paths

  # Confirmation prompt
  if [[ "$UNINSTALL_YES" != "true" ]]; then
    if [[ "$INTERACTIVE" == "true" ]]; then
      echo ""
      echo "This will remove:"
      echo "  - Binary: ${INSTALLED_BIN} (and ${PREVIOUS_BIN})"
      echo "  - Service: ${SERVICE_PATH}"
      echo "  - Config: ${ENV_PATH} (and ${INSTALLER_BACKUP_DIR})"
      echo "  - State: ${STATE_DIR}"
      if [[ "$REMOVE_BACKUPS" == "true" ]]; then
        echo "  - Backups: ${BACKUP_DIR}"
      else
        echo "  - Backups will be PRESERVED at: ${BACKUP_DIR}"
      fi
      echo ""
      read -r -p "Are you sure you want to uninstall? [y/N]: " CONFIRM
      if [[ ! "$CONFIRM" =~ ^[Yy] ]]; then
        log "Uninstall cancelled"
        exit 0
      fi
    else
      log_error "Non-interactive uninstall requires --yes flag"
      log_error "Usage: ... | bash -s -- --uninstall --yes"
      exit 1
    fi
  fi

  # Never stop the updater in the middle of an update.
  UPDATER_HTTP_PORT="$(effective_updater_port)"
  if [[ -x "$INSTALLED_BIN" ]] && "$INSTALLED_BIN" version >/dev/null 2>&1; then
    DAEMON_HEARTBEATS=true
  fi
  if ! wait_until_updater_idle; then
    abort_busy "Uninstall cancelled: nothing was removed and the updater was not stopped."
  fi

  # Stop and disable service
  if [[ "$OS" == "linux" ]]; then
    if systemctl is-active --quiet payram-updater 2>/dev/null; then
      log "Stopping payram-updater service..."
      $SUDO systemctl stop payram-updater
    fi

    if systemctl is-enabled --quiet payram-updater 2>/dev/null; then
      log "Disabling payram-updater service..."
      $SUDO systemctl disable payram-updater
    fi

    # Remove service file
    if [[ -f "$SERVICE_PATH" ]]; then
      log "Removing service file: $SERVICE_PATH"
      $SUDO rm -f "$SERVICE_PATH"
      $SUDO systemctl daemon-reload
    fi
  elif [[ "$OS" == "darwin" ]]; then
    if $SUDO launchctl list com.payram.updater >/dev/null 2>&1; then
      log "Stopping payram-updater service..."
      $SUDO launchctl unload "$SERVICE_PATH" 2>/dev/null || true
    fi

    # Remove plist file
    if [[ -f "$SERVICE_PATH" ]]; then
      log "Removing service file: $SERVICE_PATH"
      $SUDO rm -f "$SERVICE_PATH"
    fi

    # Remove launcher script
    if [[ -f "$LAUNCHER_PATH" ]]; then
      log "Removing launcher script: $LAUNCHER_PATH"
      $SUDO rm -f "$LAUNCHER_PATH"
    fi
  fi

  # Remove binaries
  local bin
  for bin in "$INSTALLED_BIN" "$PREVIOUS_BIN" "$STAGED_BIN"; do
    if [[ -f "$bin" ]]; then
      log "Removing binary: $bin"
      $SUDO rm -f "$bin"
    fi
  done

  # Remove config
  if [[ -f "$ENV_PATH" ]]; then
    log "Removing config: $ENV_PATH"
    $SUDO rm -f "$ENV_PATH"
  fi
  if [[ -d "$INSTALLER_BACKUP_DIR" ]]; then
    log "Removing config backups: $INSTALLER_BACKUP_DIR"
    $SUDO rm -rf "$INSTALLER_BACKUP_DIR"
  fi

  # Remove /etc/payram if empty
  local config_dir data_dir
  config_dir="$(dirname "$ENV_PATH")"
  data_dir="$(dirname "$BACKUP_DIR")"
  if [[ -d "$config_dir" ]] && [[ -z "$(ls -A "$config_dir" 2>/dev/null)" ]]; then
    log "Removing empty config directory: $config_dir"
    $SUDO rmdir "$config_dir"
  fi

  # Remove state directory
  if [[ -d "$STATE_DIR" ]]; then
    log "Removing state directory: $STATE_DIR"
    $SUDO rm -rf "$STATE_DIR"
  fi

  # Remove backups if requested
  if [[ "$REMOVE_BACKUPS" == "true" ]]; then
    if [[ -d "$BACKUP_DIR" ]]; then
      log "Removing backups directory: $BACKUP_DIR"
      $SUDO rm -rf "$BACKUP_DIR"
    fi
    # Remove /var/lib/payram if empty
    if [[ -d "$data_dir" ]] && [[ -z "$(ls -A "$data_dir" 2>/dev/null)" ]]; then
      log "Removing empty directory: $data_dir"
      $SUDO rmdir "$data_dir"
    fi
  else
    if [[ -d "$BACKUP_DIR" ]]; then
      log "Backups preserved at: $BACKUP_DIR"
    fi
  fi

  # Remove iptables rule if it exists (Linux only)
  if [[ "$OS" == "linux" ]] && $SUDO iptables -C INPUT -i docker0 -p tcp --dport 2567 -j ACCEPT 2>/dev/null; then
    log "Removing iptables rule for docker0..."
    $SUDO iptables -D INPUT -i docker0 -p tcp --dport 2567 -j ACCEPT || true
  fi

  log ""
  log "Uninstall complete!"
  log ""
  if [[ "$OS" == "linux" ]]; then
    log "Note: If you added firewall rules manually (ufw/firewalld), you may want to remove them:"
    log "  UFW:       sudo ufw delete allow in on docker0 to any port 2567"
    log "  firewalld: sudo firewall-cmd --zone=docker --remove-port=2567/tcp --permanent"
  fi
  exit 0
}

# ----------------------------------------------------------------------------
# Download, verify and install the binary
# ----------------------------------------------------------------------------

# macOS: root doesn't inherit the user-owned Colima Docker socket.
# Detect it now so payram-updater init (run via sudo) can reach Docker.
detect_colima_docker_host() {
  if [[ "$OS" == "darwin" && -z "${DOCKER_HOST:-}" ]]; then
    local colima_user colima_socket
    colima_user="${SUDO_USER:-$(logname 2>/dev/null || echo "")}"
    if [[ -n "$colima_user" ]]; then
      colima_socket="$(eval echo "~$colima_user")/.colima/default/docker.sock"
      if [[ -S "$colima_socket" ]]; then
        export DOCKER_HOST="unix://$colima_socket"
        log "macOS: DOCKER_HOST auto-detected ($DOCKER_HOST)"
      fi
    fi
  fi
}

resolve_version() {
  log "Fetching latest version..."
  VERSION="${PAYRAM_UPDATER_VERSION:-}"
  ASSET="${BIN_NAME}-${OS}-${ARCH}"
  local release_path
  if [[ -z "$VERSION" ]]; then
    VERSION="$(curl -fsSL --connect-timeout 15 "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/latest" 2>/dev/null | \
      grep -m1 '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/' || true)"
  fi

  if [[ -n "$VERSION" ]]; then
    log "Version to install: $VERSION"
    release_path="download/${VERSION}"
  else
    # GitHub's API allows 60 unauthenticated requests an hour per address and
    # is sometimes blocked; the release download links do not need it. The
    # binary and its checksum both come from the latest release (a release
    # published between the two downloads makes the checksum fail, safely).
    VERSION="latest"
    log "Could not ask GitHub's API for the latest version; downloading the latest release directly"
    release_path="latest/download"
  fi
  DOWNLOAD_URL="${PAYRAM_UPDATER_DOWNLOAD_URL:-https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/${release_path}/${ASSET}}"
  CHECKSUM_URL="${PAYRAM_UPDATER_CHECKSUM_URL:-${DOWNLOAD_URL}.sha256}"
}

download_binary() {
  DOWNLOADED_BIN="${WORK_DIR}/${BIN_NAME}"
  DOWNLOADED_SUM="${WORK_DIR}/${BIN_NAME}.sha256"
  log "Downloading ${DOWNLOAD_URL}..."
  if ! curl -fsSL --retry 3 --connect-timeout 15 "$DOWNLOAD_URL" -o "$DOWNLOADED_BIN"; then
    log_error "Could not download ${DOWNLOAD_URL}"
    log_error "If ${VERSION} was published in the last few minutes its binaries may still be building:"
    log_error "wait a few minutes and run this again, or install a specific release with PAYRAM_UPDATER_VERSION=<tag>."
    log_error "Nothing was changed."
    exit 1
  fi
  chmod +x "$DOWNLOADED_BIN"
  log "Download complete"
}

verify_checksum() {
  local checksum=""
  if [[ -n "${PAYRAM_UPDATER_SHA256:-}" ]]; then
    checksum="$PAYRAM_UPDATER_SHA256"
  elif curl -fsSL --retry 3 "$CHECKSUM_URL" -o "$DOWNLOADED_SUM"; then
    checksum="$(awk '{print $1}' "$DOWNLOADED_SUM" | head -n1)"
  else
    log_error "Checksum not available at ${CHECKSUM_URL}"
    exit 1
  fi

  if [[ -z "$checksum" ]]; then
    log_error "Empty checksum from ${CHECKSUM_URL}"
    exit 1
  fi

  if command -v sha256sum >/dev/null 2>&1; then
    if [[ "$QUIET" == "true" ]]; then
      echo "${checksum}  ${DOWNLOADED_BIN}" | sha256sum -c - >/dev/null
    else
      echo "${checksum}  ${DOWNLOADED_BIN}" | sha256sum -c -
    fi
  elif command -v shasum >/dev/null 2>&1; then
    if [[ "$QUIET" == "true" ]]; then
      echo "${checksum}  ${DOWNLOADED_BIN}" | shasum -a 256 -c - >/dev/null
    else
      echo "${checksum}  ${DOWNLOADED_BIN}" | shasum -a 256 -c -
    fi
  else
    log_error "sha256sum or shasum is required for checksum verification"
    exit 1
  fi
}

# Copy the verified binary next to the installed one and make sure it runs on
# this machine (right OS/arch, no missing libraries) before anything changes.
stage_and_test_binary() {
  local output
  $SUDO mkdir -p "$INSTALL_DIR"
  $SUDO install -m 0755 "$DOWNLOADED_BIN" "$STAGED_BIN"
  if output="$("$STAGED_BIN" version 2>&1)"; then
    NEW_VERSION="$(echo "$output" | head -n 1)"
  elif "$STAGED_BIN" help >/dev/null 2>&1; then
    # Releases from before the version command still run fine.
    NEW_VERSION="${VERSION} (no version command)"
  else
    log_error "The downloaded updater (${VERSION}, ${OS}-${ARCH}) does not run on this server:"
    echo "$output" | head -n 5 >&2
    log_error "Nothing was changed; the current updater keeps running."
    exit 1
  fi
  log "New binary runs on this server (version: ${NEW_VERSION})"
}

# Put back the binary this run replaced. Returns 1 when there is none.
restore_previous_binary() {
  if [[ "$BINARY_REPLACED" != "true" || "$PREVIOUS_SAVED" != "true" ]] || ! $SUDO test -f "$PREVIOUS_BIN"; then
    return 1
  fi
  $SUDO install -m 0755 "$PREVIOUS_BIN" "$STAGED_BIN"
  $SUDO mv -f "$STAGED_BIN" "$INSTALLED_BIN"
  BINARY_REPLACED=false
  log "Previous binary (version: ${CURRENT_VERSION}) is back at ${INSTALLED_BIN}"
}

install_binary() {
  if $SUDO test -f "$INSTALLED_BIN"; then
    $SUDO cp -p "$INSTALLED_BIN" "$PREVIOUS_BIN"
    PREVIOUS_SAVED=true
    log "Kept the current binary as ${PREVIOUS_BIN}"
  fi
  log "Installing binary to ${INSTALLED_BIN}..."
  $SUDO mv -f "$STAGED_BIN" "$INSTALLED_BIN"
  BINARY_REPLACED=true
  log "Binary installed successfully"
}

# ----------------------------------------------------------------------------
# Service definition (always regenerated: it is not meant to be edited; use a
# systemd drop-in under payram-updater.service.d/ for local changes)
# ----------------------------------------------------------------------------

systemd_unit_content() {
  cat <<'EOF'
# The PayRam Updater service, as installed by setup_payram_updater.sh (same as
# packaging/systemd/payram-updater.service). The installer rewrites this file
# on every run: put local changes in a drop-in (systemctl edit payram-updater).
[Unit]
Description=Payram Updater Service
Documentation=https://github.com/PayRam/payram-updates
After=docker.service network-online.target
Wants=docker.service network-online.target

[Service]
Type=simple
User=root
Group=root
# systemd's default PATH has no /snap/bin, where a Docker installed as a snap
# (offered by Ubuntu Server's installer) puts the docker command.
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin
EnvironmentFile=/etc/payram/updater.env
ExecStart=/usr/local/bin/payram-updater serve
Restart=always
RestartSec=10
# On stop, SIGTERM goes to the updater only, not to the docker commands it is
# running, and it gets up to 15 minutes to finish or roll back an update in
# progress before anything is killed.
KillMode=mixed
TimeoutStopSec=900
StandardOutput=journal
StandardError=journal
SyslogIdentifier=payram-updater

# No PrivateTmp/ProtectSystem: they fail on OpenVZ/LXC hosts, and the updater
# needs the Docker socket, its state dir and the backup dir.
NoNewPrivileges=false

[Install]
WantedBy=multi-user.target
EOF
}

launchd_launcher_content() {
  cat <<'EOF'
#!/bin/bash
set -a
source /etc/payram/updater.env
set +a
exec /usr/local/bin/payram-updater serve
EOF
}

launchd_plist_content() {
  cat <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.payram.updater</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/payram-updater-launcher</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ExitTimeOut</key>
    <integer>900</integer>
    <key>StandardOutPath</key>
    <string>/var/log/payram-updater.log</string>
    <key>StandardErrorPath</key>
    <string>/var/log/payram-updater.error.log</string>
</dict>
</plist>
EOF
}

# install_generated_file SOURCE DEST MODE: put SOURCE at DEST unless DEST
# already has exactly that content; back up the DEST being replaced.
install_generated_file() {
  local src="$1" dest="$2" mode="$3"
  if $SUDO test -f "$dest"; then
    if $SUDO cmp -s "$src" "$dest" 2>/dev/null; then
      log "$dest is up to date"
      return 0
    fi
    backup_file "$dest" 0600
  fi
  $SUDO install -m "$mode" "$src" "$dest"
  log "Wrote $dest"
}

install_service_files() {
  if [[ "$OS" == "linux" ]]; then
    log "Installing service at $SERVICE_PATH..."
    systemd_unit_content >"${WORK_DIR}/payram-updater.service"
    install_generated_file "${WORK_DIR}/payram-updater.service" "$SERVICE_PATH" 0644
    log "Reloading service manager..."
    $SUDO systemctl daemon-reload
  else
    log "Installing launchd plist at $SERVICE_PATH..."
    launchd_launcher_content >"${WORK_DIR}/payram-updater-launcher"
    install_generated_file "${WORK_DIR}/payram-updater-launcher" "$LAUNCHER_PATH" 0755
    launchd_plist_content >"${WORK_DIR}/com.payram.updater.plist"
    install_generated_file "${WORK_DIR}/com.payram.updater.plist" "$SERVICE_PATH" 0644
  fi
  log "Service installed"
}

run_updater_init() {
  local init_out
  log "Running updater init..."
  if ! init_out=$($SUDO env DOCKER_HOST="${DOCKER_HOST:-}" "$INSTALLED_BIN" init --no-autoupdate 2>&1); then
    echo "$init_out" >&2
    log_error "Initialization failed"
    if restore_previous_binary; then
      log_error "The previous updater binary was put back."
    fi
    exit 1
  fi
  log "Initialization complete"
}

# ----------------------------------------------------------------------------
# Start and verify
# ----------------------------------------------------------------------------

# The updater did not come up after the restart: put the previous binary back.
roll_back_failed_start() {
  log_error "The updater (version: ${NEW_VERSION:-unknown}) did not answer on http://127.0.0.1:${UPDATER_HTTP_PORT}/health within ${SETUP_HEALTH_WAIT_SECONDS}s of the restart."
  if restore_previous_binary; then
    log_error "Restarting the previous updater (version: ${CURRENT_VERSION})..."
    restart_updater_service || true
    if wait_for_updater_health; then
      log_error "The previous updater is running again. The new version was not kept. PayRam itself was not touched."
    else
      log_error "The previous updater did not answer either. PayRam itself was not touched, but the updater needs a look."
    fi
  else
    log_error "There is no previous updater binary to go back to. PayRam itself was not touched."
  fi
  log_error "Service logs: $(service_logs_hint)"
  exit 1
}

# The PayRam container the updater manages: TARGET_CONTAINER_NAME if set,
# otherwise the running container of the PayRam image with the highest
# version (the same choice the updater makes for its access control).
discover_payram_container() {
  local docker_bin="$1" target repo
  target="$(env_file_value TARGET_CONTAINER_NAME)"
  target="${target:-${TARGET_CONTAINER_NAME:-}}"
  if [[ -n "$target" ]]; then
    echo "$target"
    return 0
  fi
  repo="$(env_file_value IMAGE_REPO_OVERRIDE)"
  repo="${repo:-${IMAGE_REPO_OVERRIDE:-payramapp/payram}}"
  $SUDO "$docker_bin" ps --format '{{.Names}} {{.Image}}' 2>/dev/null | awk -v p="${repo}:" '
    index($2, p) == 1 {
      tag = substr($2, length(p) + 1)
      # version tags sort above "latest" and other names
      print ((tag ~ /^v?[0-9]/) ? 1 : 0), tag, $1
    }' | sort -k1,1n -k2,2V 2>/dev/null | tail -n 1 | awk '{ print $3 }' || true
}

# run_with_timeout SECONDS COMMAND...: COMMAND, killed after SECONDS when
# timeout(1) is available.
run_with_timeout() {
  local seconds="$1"
  shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "$seconds" "$@"
  else
    "$@"
  fi
}

# Linux: check that the PayRam container can reach the updater through the
# Docker bridge (firewalls such as ufw/firewalld often block it).
check_container_connectivity() {
  local docker_bin target bridge_ip url
  docker_bin="$(env_file_value DOCKER_BIN)"
  docker_bin="${docker_bin:-${DOCKER_BIN:-docker}}"
  if ! command -v "$docker_bin" >/dev/null 2>&1; then
    return 0
  fi

  target="$(discover_payram_container "$docker_bin")"
  if [[ -z "$target" ]] || ! $SUDO "$docker_bin" ps --format '{{.Names}}' 2>/dev/null | grep -qx "$target"; then
    log "No running PayRam container found; skipping the container-to-updater connectivity test"
    return 0
  fi

  bridge_ip="$(ip -4 addr show docker0 2>/dev/null | awk '/inet / { sub(/\/.*/, "", $2); print $2; exit }' || true)"
  bridge_ip="${bridge_ip:-172.17.0.1}"
  url="http://${bridge_ip}:${UPDATER_HTTP_PORT}/health"

  if ! $SUDO "$docker_bin" exec "$target" sh -c 'command -v wget || command -v curl' >/dev/null 2>&1; then
    log "Container ${target} has neither wget nor curl; skipping the container-to-updater connectivity test"
    return 0
  fi

  log "Testing connectivity from container ${target} to the updater (${url})..."
  # Try wget first, then curl
  if run_with_timeout 5 $SUDO "$docker_bin" exec "$target" wget -qO- "$url" >/dev/null 2>&1 || \
     run_with_timeout 5 $SUDO "$docker_bin" exec "$target" curl -sf "$url" >/dev/null 2>&1; then
    log "Connectivity test passed"
  else
    log ""
    log "WARNING: Container ${target} cannot reach the updater API (${url})."
    log "The dashboard's Update button will not work until this is fixed."
    log "This is usually caused by firewall rules blocking docker0 traffic."
    log ""
    log "To fix, run ONE of the following based on your firewall:"
    if command -v ufw >/dev/null 2>&1; then
      log "  UFW:       sudo ufw allow in on docker0 to any port ${UPDATER_HTTP_PORT}"
    fi
    if command -v firewall-cmd >/dev/null 2>&1; then
      log "  firewalld: sudo firewall-cmd --zone=docker --add-port=${UPDATER_HTTP_PORT}/tcp --permanent && sudo firewall-cmd --reload"
    fi
    log "  iptables:  sudo iptables -I INPUT -i docker0 -p tcp --dport ${UPDATER_HTTP_PORT} -j ACCEPT"
    log ""
    log "See troubleshooting docs for more details."
  fi
}

start_and_verify_service() {
  if [[ "$OS" == "linux" ]]; then
    log "Enabling payram-updater service..."
    if [[ "$QUIET" == "true" ]]; then
      $SUDO systemctl enable "$SERVICE_NAME" >/dev/null 2>&1
    else
      $SUDO systemctl enable "$SERVICE_NAME"
    fi
  fi

  # Last check right before the restart: an update may have started meanwhile.
  if ! wait_until_updater_idle; then
    if restore_previous_binary; then
      abort_busy "The previous binary was put back and the running updater was not restarted."
    fi
    abort_busy "The running updater was not restarted."
  fi

  log "Restarting payram-updater service..."
  if ! restart_updater_service; then
    log_error "The service manager could not restart ${SERVICE_NAME}"
  fi

  log "Waiting for the updater to answer on 127.0.0.1:${UPDATER_HTTP_PORT} (up to ${SETUP_HEALTH_WAIT_SECONDS}s)..."
  if ! wait_for_updater_health; then
    roll_back_failed_start
  fi
  log "Updater is running (version: ${NEW_VERSION:-unknown})"

  if [[ "$OS" == "linux" ]]; then
    check_container_connectivity
  fi
}

main() {
  parse_args "$@"
  detect_os

  # Handle uninstall mode
  if [[ "$UNINSTALL_MODE" == "true" ]]; then
    do_uninstall
  fi

  log "Starting PayRam Updater setup..."

  log "Checking dependencies..."
  require curl
  if [[ -n "$SUDO" ]]; then
    require sudo
  fi

  log "Detecting operating system..."
  log "Detected OS: $OS"

  log "Detecting system architecture..."
  detect_arch
  log "Detected: ${OS}-${ARCH}"

  detect_colima_docker_host
  set_binary_paths

  WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/payram-updater-setup.XXXXXXXX")"
  trap cleanup_work_files EXIT

  resolve_version
  download_binary
  log "Verifying checksum..."
  verify_checksum
  log "Checksum verification complete"
  stage_and_test_binary

  local replace_binary="y"
  if $SUDO test -f "$INSTALLED_BIN"; then
    # Releases before the version command print their help and exit 1.
    if CURRENT_VERSION="$("$INSTALLED_BIN" version 2>/dev/null)" && [[ -n "$CURRENT_VERSION" ]]; then
      CURRENT_VERSION="$(echo "$CURRENT_VERSION" | head -n 1)"
      # Only releases that report a version save active jobs as a heartbeat.
      DAEMON_HEARTBEATS=true
    else
      CURRENT_VERSION="unknown (older release)"
    fi
    log "Existing binary found (version: $CURRENT_VERSION)"

    if [[ "$FORCE_REINSTALL" == "true" ]]; then
      log "Force reinstall mode - overriding existing binary"
    elif [[ "$INTERACTIVE" == "true" ]]; then
      read -r -p "Override existing binary? [Y/n]: " replace_binary
    else
      log "Non-interactive mode - overriding existing binary"
    fi
  fi

  # Nothing is changed while an update is running: it waits, or gives up.
  UPDATER_HTTP_PORT="$(effective_updater_port)"
  if [[ "$ENABLE_SERVICE" == "true" ]] && ! wait_until_updater_idle; then
    abort_busy "Nothing was changed: the running updater, its binary and its settings were not touched."
  fi

  if [[ ! "$replace_binary" =~ ^[Nn] ]]; then
    install_binary
  else
    log "Keeping existing binary"
    NEW_VERSION="$CURRENT_VERSION"
  fi

  log "Creating required directories..."
  $SUDO mkdir -p "$(dirname "$ENV_PATH")"
  $SUDO mkdir -p "$STATE_DIR" "$BACKUP_DIR"
  # Root only: backups hold the whole payments database, and the state
  # directory holds the update log and the CLI's token.
  $SUDO chmod 0700 "$STATE_DIR" "$BACKUP_DIR"
  log "Directories created"

  write_env_file

  # macOS: persist DOCKER_HOST (Colima socket) into the env file so the
  # launchd service (running as root) can reach the user-owned Docker daemon.
  if [[ "$OS" == "darwin" && -n "${DOCKER_HOST:-}" ]]; then
    if ! $SUDO grep -q "^DOCKER_HOST=" "$ENV_PATH" 2>/dev/null; then
      echo "DOCKER_HOST=${DOCKER_HOST}" | $SUDO tee -a "$ENV_PATH" >/dev/null
      log "macOS: Persisted DOCKER_HOST=$DOCKER_HOST to $ENV_PATH"
    fi
  fi

  install_service_files

  log "Checking initialization status..."
  local reinit="n"
  if $SUDO test -f "$INIT_CONFIG" && $SUDO grep -q '"initialized": true' "$INIT_CONFIG"; then
    log "Updater already initialized"

    if [[ "$FORCE_REINSTALL" == "true" ]]; then
      log "Force reinstall mode - skipping re-initialization"
    elif [[ "$INTERACTIVE" == "true" ]]; then
      read -r -p "Re-run initialization? [y/N]: " reinit
    else
      log "Non-interactive mode - skipping re-initialization"
    fi

    if [[ "$reinit" =~ ^[Yy] ]]; then
      run_updater_init
    else
      log "Skipping initialization"
    fi
  else
    run_updater_init
  fi

  # The env file now has UPDATER_PORT; that is what the restarted updater uses.
  UPDATER_HTTP_PORT="$(effective_updater_port)"

  if [[ "$ENABLE_SERVICE" == "true" ]]; then
    start_and_verify_service
  else
    log "Service installed but NOT started (ENABLE_SERVICE is not set)."
    if [[ "$OS" == "linux" ]]; then
      log "To enable and start the service, run:"
      log "  sudo systemctl enable payram-updater"
      log "  sudo systemctl start payram-updater"
    else
      log "To load the service, run:"
      log "  sudo launchctl load $SERVICE_PATH"
    fi
  fi

  log "Setup complete! (updater version: ${NEW_VERSION:-unknown})"
  if [[ "$OS" == "darwin" ]]; then
    log "Logs: /var/log/payram-updater.log and /var/log/payram-updater.error.log"
  fi
}

# Tests source this file with PAYRAM_UPDATER_SETUP_LIBRARY_ONLY=true to call
# its functions one at a time.
if [[ "${PAYRAM_UPDATER_SETUP_LIBRARY_ONLY:-}" != "true" ]]; then
  main "$@"
fi
