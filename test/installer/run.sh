#!/usr/bin/env bash
# Installer test: runs setup_payram_updater.sh from this checkout on fresh
# Linux servers and checks that it installs or upgrades the updater without
# ever leaving a server without a working one.
#
# Each server is a privileged container that boots systemd and runs its own
# Docker daemon (so nothing touches the Docker running this script), with a
# stand-in PayRam container (busybox tagged payramapp/payram) for the
# installer's container-to-updater connectivity test. The updater built from
# this checkout is "downloaded" from a file:// URL; the released v0.2.3 and
# the released installer come from GitHub, as merchants get them.
#
# Usage: test/installer/run.sh [case ...]   (default: all, in order)
#   INSTALLER_TEST_DIR  working directory (default: $TMPDIR/payram-updater-installer-test)
#   INSTALLER_DISTROS   base images for the fresh-install case
#                       (default: ubuntu:24.04 debian:12 ubuntu:20.04)
# Needs Docker (privileged containers), Go, internet access to GitHub and Docker Hub.
set -uo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
W="${INSTALLER_TEST_DIR:-${TMPDIR:-/tmp}/payram-updater-installer-test}"
mkdir -p "$W"
W="$(cd "$W" && pwd)"
PREFIX=payram-installer
DISTROS="${INSTALLER_DISTROS:-ubuntu:24.04 debian:12 ubuntu:20.04}"
TEST_VERSION=v0.3.0-installer-test
RELEASED_INSTALLER=https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh

log() { printf '\n[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
pass() { printf '  PASS %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*"; FAILURES=$((FAILURES + 1)); }
info() { printf '  INFO %s\n' "$*"; }
FAILURES=0

case "$(docker info -f '{{.Architecture}}' 2>/dev/null)" in
  aarch64|arm64) GOARCH=arm64 ;;
  *) GOARCH=amd64 ;;
esac

# ---------------------------------------------------------------------------
# Test binaries and server images
# ---------------------------------------------------------------------------

build_binaries() {
  log "Building the updater for linux/$GOARCH"
  (cd "$REPO" && CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -trimpath \
    -ldflags "-X main.version=$TEST_VERSION" -o "$W/payram-updater" ./cmd/payram-updater) || { echo "build failed"; exit 1; }
  SHA="$(shasum -a 256 "$W/payram-updater" | awk '{print $1}')"
  # A build that installs and answers "version" but cannot serve: the
  # installer must put the previous updater back.
  printf '%s\n' '#!/bin/sh' 'case "$1" in version) echo v9.9.9-broken ;; serve) echo "broken build" >&2; exit 1 ;; esac; exit 0' > "$W/payram-updater-broken"
  chmod 755 "$W/payram-updater-broken"
  BROKEN_SHA="$(shasum -a 256 "$W/payram-updater-broken" | awk '{print $1}')"
}

# image_for DISTRO SUDO: the server image (systemd + Docker + curl), with or
# without sudo and an "admin" user allowed to use it.
image_for() {
  local distro="$1" with_sudo="$2" tag
  tag="$PREFIX-test:$(echo "$distro" | tr ':' '-')-$([ "$with_sudo" = yes ] && echo sudo || echo root)"
  if ! docker image inspect "$tag" >/dev/null 2>&1; then
    local extra="" user=""
    if [ "$with_sudo" = yes ]; then
      extra="sudo"
      user='&& useradd -m -s /bin/bash admin && echo "admin ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/admin'
    fi
    printf '%s\n' "FROM $distro" \
      "RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends systemd systemd-sysv docker.io curl ca-certificates iproute2 procps iptables $extra && rm -rf /var/lib/apt/lists/* $user" \
      'STOPSIGNAL SIGRTMIN+3' 'CMD ["/sbin/init"]' | docker build -q -t "$tag" - >/dev/null || { echo "could not build $tag"; exit 1; }
  fi
  echo "$tag"
}

# ---------------------------------------------------------------------------
# Servers
# ---------------------------------------------------------------------------

# server_up NAME IMAGE [docker run options...]: boot a server with Docker and a
# running stand-in PayRam, and copy the installer and the test binaries in.
server_up() {
  local short="$1" name="$PREFIX-$1" image="$2"
  shift 2
  server_down "$short"
  docker run -d --name "$name" --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
    -v "$name-docker:/var/lib/docker" -v "$name-containerd:/var/lib/containerd" "$@" "$image" >/dev/null || { fail "could not start server $name"; return 1; }
  docker exec "$name" systemctl is-system-running --wait >/dev/null 2>&1
  for _ in $(seq 1 60); do docker exec "$name" docker info >/dev/null 2>&1 && break; sleep 2; done
  docker exec "$name" sh -c 'docker pull -q busybox:1.36 >/dev/null && docker tag busybox:1.36 payramapp/payram:3.10.0 &&
    docker run -d --name payram --restart unless-stopped payramapp/payram:3.10.0 sleep 1d >/dev/null' || { fail "no stand-in PayRam on $name"; return 1; }
  docker exec "$name" mkdir -p /opt/test
  docker cp "$REPO/setup_payram_updater.sh" "$name:/opt/test/setup_payram_updater.sh" >/dev/null
  docker cp "$W/payram-updater" "$name:/opt/test/payram-updater" >/dev/null
  docker cp "$W/payram-updater-broken" "$name:/opt/test/payram-updater-broken" >/dev/null
  pass "server $short up ($image)"
}

server_down() {
  local name="$PREFIX-$1"
  docker rm -f "$name" >/dev/null 2>&1
  docker volume rm "$name-docker" "$name-containerd" >/dev/null 2>&1
}

# on SERVER COMMAND: run a shell command on the server as root.
on() { local s="$1"; shift; docker exec "$PREFIX-$s" bash -c "$*"; }

NEW_ENV="PAYRAM_UPDATER_VERSION=$TEST_VERSION PAYRAM_UPDATER_DOWNLOAD_URL=file:///opt/test/payram-updater"

# install SERVER LOG [env...]: run this checkout's installer as root, piped
# the way merchants run it (curl ... | sudo bash). Returns its exit code.
install() {
  local s="$1" out="$W/$1-$2.log"
  shift 2
  on "$s" "curl -fsSL file:///opt/test/setup_payram_updater.sh | env $NEW_ENV PAYRAM_UPDATER_SHA256=$SHA $* bash" >"$out" 2>&1
}

expect_exit() { # WANT GOT WHAT LOG
  if [ "$2" = "$1" ]; then pass "$3 (exit $2)"; else fail "$3: exit $2, want $1 (see $4)"; tail -15 "$4" | sed 's/^/      /'; fi
}

# expect_updater SERVER VERSION [PORT]: the service runs, is enabled, answers
# /health and the installed binary is VERSION ("released" = a published
# release, not this checkout's build).
expect_updater() {
  local s="$1" want="$2" port="${3:-2567}" got
  on "$s" "systemctl is-active --quiet payram-updater && systemctl is-enabled --quiet payram-updater" \
    && pass "service active and enabled" || fail "service not active/enabled: $(on "$s" 'systemctl status payram-updater --no-pager 2>&1 | tail -5')"
  on "$s" "for i in \$(seq 1 30); do curl -fsS -m 2 http://127.0.0.1:$port/health 2>/dev/null | grep -q ok && exit 0; sleep 1; done; exit 1" \
    && pass "updater answers on 127.0.0.1:$port" || fail "updater does not answer on $port"
  if [ "$want" = released ]; then
    got="$(on "$s" '/usr/local/bin/payram-updater version 2>/dev/null')" || got="(release without a version command)"
    [ "$got" != "$TEST_VERSION" ] && on "$s" '/usr/local/bin/payram-updater help 2>&1 | grep -q payram-updater' \
      && pass "released updater installed ($got)" || fail "installed binary is not a released one ($got)"
  else
    got="$(on "$s" '/usr/local/bin/payram-updater version' 2>/dev/null)"
    [ "$got" = "$want" ] && pass "installed version $got" || fail "installed version '$got', want $want"
  fi
}

expect_mode() { # SERVER PATH MODE
  local got; got="$(on "$1" "stat -c %a '$2'" 2>/dev/null)"
  [ "$got" = "$3" ] && pass "$2 mode $3" || fail "$2 mode '$got', want $3"
}

expect_env() { # SERVER LINE: the env file has exactly this line
  on "$1" "grep -qxF -- '$2' /etc/payram/updater.env" && pass "env file has $2" || fail "env file lacks $2"
}

expect_log() { # LOG TEXT
  grep -qF -- "$2" "$1" && pass "installer said: $2" || fail "installer never said '$2' (see $1)"
}

# ---------------------------------------------------------------------------
# Cases
# ---------------------------------------------------------------------------

# A new server where the merchant logs in as root and sudo is not installed
# (Debian images on many hosts). The released installer stops at "Missing
# dependency: sudo", and setup_payram.sh hides that, so such servers have
# PayRam but no updater: the dashboard's Update button never works.
case_fresh_install_as_root_without_sudo() {
  local distro tag s
  for distro in $DISTROS; do
    s="fresh-$(echo "$distro" | tr ':.' '--')"
    log "--- $distro ---"
    tag="$(image_for "$distro" no)"
    server_up "$s" "$tag" || continue
    on "$s" "command -v sudo" >/dev/null && fail "$distro image has sudo" || pass "no sudo on this server"
    on "$s" "curl -fsSL $RELEASED_INSTALLER | bash" >"$W/$s-released.log" 2>&1
    local released=$?
    if [ $released -ne 0 ] && grep -q "Missing dependency: sudo" "$W/$s-released.log"; then
      info "the released installer fails here ('Missing dependency: sudo'): the case this release fixes"
    else
      info "the released installer exited $released here"
    fi
    install "$s" install; expect_exit 0 $? "installer succeeds as root without sudo" "$W/$s-install.log"
    expect_updater "$s" "$TEST_VERSION"
    expect_env "$s" "EXECUTION_MODE=execute"
    expect_env "$s" "HEALTH_CHECK_TIMEOUT_SECONDS=600"
    expect_mode "$s" /var/lib/payram-updater 700
    expect_mode "$s" /var/lib/payram/backups 700
    expect_mode "$s" /var/lib/payram-updater/cli-token 600
    expect_log "$W/$s-install.log" "Connectivity test passed"
    on "$s" "/usr/local/bin/payram-updater status" >/dev/null 2>&1 && pass "payram-updater status works" || fail "payram-updater status fails"
    server_down "$s"
  done
}

# The merchant's server today: v0.2.3 installed by the released installer.
# Re-running the installer (the release notice's one command) must upgrade
# it and keep the merchant's settings; running it again must change nothing;
# it must wait for an update in progress; a new build that cannot start must
# be replaced by the previous one.
case_upgrade_over_released_v0_2_3() {
  local s=upgrade tag
  tag="$(image_for ubuntu:24.04 yes)"
  server_up "$s" "$tag" || return
  on "$s" "curl -fsSL $RELEASED_INSTALLER | bash" >"$W/$s-released.log" 2>&1
  expect_exit 0 $? "released installer installs v0.2.3" "$W/$s-released.log"
  expect_updater "$s" released
  # The merchant's own settings: one added, one changed.
  on "$s" "echo 'TARGET_CONTAINER_NAME=payram' >> /etc/payram/updater.env; sed -i 's/^FETCH_TIMEOUT_SECONDS=.*/FETCH_TIMEOUT_SECONDS=25/' /etc/payram/updater.env; systemctl restart payram-updater"
  sleep 2

  log "upgrade to this checkout"
  install "$s" upgrade; expect_exit 0 $? "installer upgrades v0.2.3" "$W/$s-upgrade.log"
  expect_updater "$s" "$TEST_VERSION"
  expect_env "$s" "TARGET_CONTAINER_NAME=payram"
  expect_env "$s" "FETCH_TIMEOUT_SECONDS=25"
  [ "$(on "$s" "grep -c '^FETCH_TIMEOUT_SECONDS=' /etc/payram/updater.env")" = 1 ] && pass "the changed setting is not added a second time" || fail "FETCH_TIMEOUT_SECONDS appears twice"
  on "$s" "ls /etc/payram/installer-backups/updater.env.* >/dev/null 2>&1" && pass "env file backed up before it was changed" || info "env file had every setting (no backup needed)"
  on "$s" "test -x /usr/local/bin/payram-updater.previous && ! /usr/local/bin/payram-updater.previous version >/dev/null 2>&1" \
    && pass "v0.2.3 kept as payram-updater.previous" || fail "previous binary not kept"
  expect_mode "$s" /var/lib/payram/backups 700
  expect_log "$W/$s-upgrade.log" "Connectivity test passed"

  log "run it again"
  local before; before="$(on "$s" 'md5sum /etc/payram/updater.env /etc/systemd/system/payram-updater.service')"
  install "$s" rerun; expect_exit 0 $? "a second run succeeds" "$W/$s-rerun.log"
  [ "$(on "$s" 'md5sum /etc/payram/updater.env /etc/systemd/system/payram-updater.service')" = "$before" ] \
    && pass "a second run changes no setting and no service file" || fail "a second run changed the env or service file"
  expect_updater "$s" "$TEST_VERSION"

  log "an update is in progress"
  on "$s" "mkdir -p /var/lib/payram-updater/jobs/latest && printf '{\"jobId\":\"job-busy\",\"state\":\"VERIFYING\",\"requestedTarget\":\"3.11.0\",\"updatedAt\":\"%s\"}' \$(date -u +%Y-%m-%dT%H:%M:%SZ) > /var/lib/payram-updater/jobs/latest/status.json"
  local sum; sum="$(on "$s" 'md5sum /usr/local/bin/payram-updater')"
  install "$s" busy SETUP_BUSY_WAIT_SECONDS=12 SETUP_BUSY_POLL_SECONDS=4; expect_exit 1 $? "installer gives up while an update runs" "$W/$s-busy.log"
  expect_log "$W/$s-busy.log" "Nothing was changed"
  [ "$(on "$s" 'md5sum /usr/local/bin/payram-updater')" = "$sum" ] && pass "binary untouched during the update" || fail "binary replaced during an update"
  on "$s" "rm -f /var/lib/payram-updater/jobs/latest/status.json"

  log "the new build cannot start"
  on "$s" "curl -fsSL file:///opt/test/setup_payram_updater.sh | env PAYRAM_UPDATER_VERSION=v9.9.9 PAYRAM_UPDATER_DOWNLOAD_URL=file:///opt/test/payram-updater-broken PAYRAM_UPDATER_SHA256=$BROKEN_SHA SETUP_HEALTH_WAIT_SECONDS=20 bash" >"$W/$s-broken.log" 2>&1
  expect_exit 1 $? "installer reports the failed start" "$W/$s-broken.log"
  expect_log "$W/$s-broken.log" "The previous updater is running again"
  expect_updater "$s" "$TEST_VERSION"
  server_down "$s"
}

# Exactly how setup_payram.sh installs the updater after installing PayRam:
# quiet, forced, from a terminal, with its output thrown away.
case_called_by_setup_payram() {
  local s=setup-payram tag
  tag="$(image_for ubuntu:24.04 no)"
  server_up "$s" "$tag" || return
  docker exec -t "$PREFIX-$s" bash -c "curl -fsSL file:///opt/test/setup_payram_updater.sh -o /tmp/u.sh && timeout 300 env $NEW_ENV PAYRAM_UPDATER_SHA256=$SHA FORCE_REINSTALL=true QUIET=true ENABLE_SERVICE=true INIT_FLAGS=--no-autoupdate bash /tmp/u.sh >/dev/null 2>&1" </dev/null
  expect_exit 0 $? "installed the way setup_payram.sh calls it" "(output discarded, as setup_payram.sh does)"
  expect_updater "$s" "$TEST_VERSION"
  docker exec -t "$PREFIX-$s" bash -c "timeout 300 env $NEW_ENV PAYRAM_UPDATER_SHA256=$SHA FORCE_REINSTALL=true QUIET=true ENABLE_SERVICE=true bash /tmp/u.sh >/dev/null 2>&1" </dev/null
  expect_exit 0 $? "setup_payram.sh's re-run (update or restart) succeeds" "(output discarded)"
  expect_updater "$s" "$TEST_VERSION"

  log "uninstall, then install again"
  on "$s" "bash /opt/test/setup_payram_updater.sh --uninstall --yes" >"$W/$s-uninstall.log" 2>&1
  expect_exit 0 $? "uninstall succeeds" "$W/$s-uninstall.log"
  on "$s" "! systemctl is-active --quiet payram-updater && test ! -e /usr/local/bin/payram-updater && test ! -e /etc/payram/updater.env && test ! -e /var/lib/payram-updater" \
    && pass "service, binary, settings and state removed" || fail "uninstall left something behind"
  on "$s" "test -d /var/lib/payram/backups" && pass "backups kept" || fail "backups removed"
  install "$s" reinstall; expect_exit 0 $? "reinstall after uninstall" "$W/$s-reinstall.log"
  expect_updater "$s" "$TEST_VERSION"
  server_down "$s"
}

# A merchant who moved the updater to another port keeps it.
case_custom_port_kept() {
  local s=custom-port tag
  tag="$(image_for ubuntu:24.04 no)"
  server_up "$s" "$tag" || return
  on "$s" "mkdir -p /etc/payram && printf 'UPDATER_PORT=3000\n' > /etc/payram/updater.env"
  install "$s" install; expect_exit 0 $? "installer succeeds with an existing env file" "$W/$s-install.log"
  expect_env "$s" "UPDATER_PORT=3000"
  [ "$(on "$s" "grep -c '^UPDATER_PORT=' /etc/payram/updater.env")" = 1 ] && pass "UPDATER_PORT not added a second time" || fail "UPDATER_PORT added twice"
  expect_env "$s" "EXECUTION_MODE=execute"
  expect_updater "$s" "$TEST_VERSION" 3000
  expect_log "$W/$s-install.log" "Connectivity test passed"
  server_down "$s"
}

# GitHub's API refuses (60 requests an hour per address, or blocked): the
# installer downloads the latest release directly and still installs.
case_github_api_unreachable() {
  local s=no-api tag
  tag="$(image_for ubuntu:24.04 no)"
  server_up "$s" "$tag" --add-host api.github.com:127.0.0.1 || return
  on "$s" "curl -fsSL file:///opt/test/setup_payram_updater.sh | bash" >"$W/$s-install.log" 2>&1
  expect_exit 0 $? "installer installs the latest release without GitHub's API" "$W/$s-install.log"
  expect_log "$W/$s-install.log" "downloading the latest release directly"
  expect_updater "$s" released
  server_down "$s"
}

# An admin user with sudo runs the installer without sudo in front.
case_non_root_user_with_sudo() {
  local s=sudo-user tag
  tag="$(image_for debian:12 yes)"
  server_up "$s" "$tag" || return
  docker exec -u admin "$PREFIX-$s" bash -c "cd /tmp && env $NEW_ENV PAYRAM_UPDATER_SHA256=$SHA bash /opt/test/setup_payram_updater.sh" >"$W/$s-install.log" 2>&1
  expect_exit 0 $? "installer succeeds for a user with sudo" "$W/$s-install.log"
  expect_updater "$s" "$TEST_VERSION"
  docker exec -u admin "$PREFIX-$s" payram-updater dry-run --mode manual --to 3.10.0 >"$W/$s-manual.log" 2>&1
  grep -q "sudo" "$W/$s-manual.log" && pass "manual mode without sudo is refused with a hint to use sudo" || fail "manual mode without sudo: $(tail -2 "$W/$s-manual.log")"
  server_down "$s"
}

build_binaries
[ $# -eq 0 ] && set -- fresh_install_as_root_without_sudo upgrade_over_released_v0_2_3 called_by_setup_payram custom_port_kept github_api_unreachable non_root_user_with_sudo
for c in "$@"; do
  log "=== $c ==="
  "case_$c"
done
log "Done: $FAILURES failure(s)"
exit $FAILURES
