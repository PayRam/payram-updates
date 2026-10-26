#!/usr/bin/env bash
# End-to-end test of payram-updater against real PayRam containers.
#
# It runs PayRam (real released images) in a container named payram-e2e on
# 127.0.0.1:18080, drives the updater built from this checkout through
# updates that succeed, fail at each step, get killed or stopped midway, and
# through restores, and after every scenario checks the promise: exactly one
# PayRam container running and answering, with its restart policy, nothing
# else able to start on its own, and the database's marker row intact.
#
# Test-only images (a version whose API never answers, one that exits) are
# built locally under payram-e2e/payram; nothing touches payramapp/payram
# tags or any other container. Needs Docker, Go, curl, python3, ~8 GB disk.
#
# Usage: test/e2e/run.sh [scenario ...]   (default: all, in order)
#   E2E_WORK_DIR   working directory (default: $TMPDIR/payram-updater-e2e)
#   E2E_OLD/E2E_NEW  released versions to test (default 3.8.1 / 3.10.0)
set -uo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
E="${E2E_WORK_DIR:-${TMPDIR:-/tmp}/payram-updater-e2e}"
mkdir -p "$E"
E="$(cd "$E" && pwd)"
BIN="$E/payram-updater"
NAME=payram-e2e
UPORT=25670
HPORT=18080
SPORT=18443
AES=6b1f0e1d2c3b4a5968778695a4b3c2d1e0f1a2b3c4d5e6f708192a3b4c5d6e7f
HEALTH_URL="http://127.0.0.1:$HPORT"
REAL_DOCKER="${REAL_DOCKER:-$(command -v docker)}"
export REAL_DOCKER
OLD="${E2E_OLD:-3.8.1}"
NEW="${E2E_NEW:-3.10.0}"
case "$(uname -m)" in arm64|aarch64) ARCH="-arm64" ;; *) ARCH="" ;; esac
export E2E_FAIL_DIR="$E/fail"
export E2E_ARGV_LOG="$E/docker-argv.log"
: > "$E2E_ARGV_LOG"
mkdir -p "$E2E_FAIL_DIR" "$E/state" "$E/backups"
cp "$REPO/test/e2e/docker-shim.sh" "$E/docker" && chmod 755 "$E/docker"
(cd "$REPO" && go build -o "$BIN" ./cmd/payram-updater) || { echo "build failed"; exit 1; }

log() { printf '\n[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
pass() { printf '  PASS %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*"; FAILURES=$((FAILURES + 1)); }
FAILURES=0

write_config() {
  # 99.0.x are the test-only versions: 99.0.1 never answers its health
  # check, 99.0.2 exits on start, 99.0.9 does not exist anywhere.
  cat > "$E/policy.json" <<JSON
{"schema_version":1,"latest":"$NEW","updater_api_init_version":"1.9.6","arch_support":{"arm64":"1.9.1"},"breakpoints":[],
 "releases":["99.0.9","99.0.2","99.0.1","$NEW","$OLD"]}
JSON
  cat > "$E/manifest.json" <<'JSON'
{"image":{"repo":"payramapp/payram"},"health_check":{"endpoint":"/api/v1/health"}}
JSON
}

health() { curl -sk -m 3 "$HEALTH_URL/api/v1/health"; }

wait_payram_healthy() {
  local deadline=$((SECONDS + ${1:-600}))
  while [ $SECONDS -lt $deadline ]; do
    health | grep -q '"status":"ok"' && return 0
    sleep 3
  done
  return 1
}

# make_certs: a self-signed certificate where PayRam's nginx looks for one
# (SSL_CERT_PATH/fullchain.pem and privkey.pem), as Let's Encrypt leaves it.
make_certs() {
  [ -f "$E/certs/fullchain.pem" ] && return 0
  mkdir -p "$E/certs"
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj /CN=localhost \
    -keyout "$E/certs/privkey.pem" -out "$E/certs/fullchain.pem" >/dev/null 2>&1
}

# fresh_payram VERSION [LAYOUT] starts PayRam the way an install does:
#   default     named volumes; port 80 on 127.0.0.1:$HPORT and the database
#               port published (as installs before 28 May 2026 did)
#   setup       as setup_payram.sh runs it on a server with a certificate:
#               host folders for /root/payram and the logs, the certificate
#               folder read-only, ports 80 and 443, SSL_CERT_PATH set
#   https       like setup, with only 443 published (PayRam answers on HTTPS
#               only, with a certificate that does not name 127.0.0.1)
#   autoremove  started with --rm, as the DigitalOcean image's systemd unit does
fresh_payram() {
  local version="$1" layout="${2:-default}"
  log "Fresh PayRam $version ($layout layout)"
  $REAL_DOCKER rm -f $NAME $NAME-next $NAME-previous $NAME-failed >/dev/null 2>&1
  $REAL_DOCKER ps -a --format '{{.Names}}' | grep "^$NAME-previous-" | xargs -r $REAL_DOCKER rm -f >/dev/null 2>&1
  $REAL_DOCKER volume rm payram-e2e-core payram-e2e-log payram-e2e-db >/dev/null 2>&1
  rm -rf "$E/state" "$E/backups" "$E/host"; mkdir -p "$E/state" "$E/backups"
  local opts=() ssl=""
  case "$layout" in
    default)
      opts=(--restart unless-stopped -p "127.0.0.1:$HPORT:80" -p 127.0.0.1:15432:5432
            -v payram-e2e-core:/root/payram -v payram-e2e-log:/var/log)
      HEALTH_URL="http://127.0.0.1:$HPORT" ;;
    setup|https)
      make_certs; mkdir -p "$E/host/core" "$E/host/log"; ssl=/etc/letsencrypt
      opts=(--restart unless-stopped -v "$E/host/core:/root/payram" -v "$E/host/log:/var/log" -v "$E/certs:/etc/letsencrypt:ro")
      if [ "$layout" = setup ]; then
        opts+=(-p "127.0.0.1:$HPORT:80" -p "127.0.0.1:$SPORT:443" -p 127.0.0.1:15432:5432)
      else
        opts+=(-p "127.0.0.1:$SPORT:443")
      fi
      HEALTH_URL="https://127.0.0.1:$SPORT" ;;
    autoremove)
      opts=(--rm -p "127.0.0.1:$HPORT:80" -v payram-e2e-core:/root/payram -v payram-e2e-log:/var/log)
      HEALTH_URL="http://127.0.0.1:$HPORT" ;;
  esac
  $REAL_DOCKER run -d --name $NAME "${opts[@]}" \
    -e AES_KEY=$AES \
    -e BLOCKCHAIN_NETWORK_TYPE=testnet -e SERVER=DEVELOPMENT -e POSTGRES_SSLMODE=disable \
    -e POSTGRES_HOST=localhost -e POSTGRES_PORT=5432 -e POSTGRES_DATABASE=payram \
    -e POSTGRES_USERNAME=payram -e POSTGRES_PASSWORD=payram123 -e SSL_CERT_PATH=$ssl \
    -e PAYMENTS_APP_SERVER_URL=https://x.payram.com -e PAYRAM_E2E_EMPTY= \
    -v payram-e2e-db:/var/lib/payram/db/postgres \
    payram-e2e/payram:$version >/dev/null || { fail "could not start PayRam $version"; return 1; }
  if wait_payram_healthy 600; then pass "PayRam $version healthy"; else fail "PayRam $version never healthy"; return 1; fi
  sql "create table if not exists e2e_marker(id int); insert into e2e_marker values (42);" >/dev/null
}

sql() { $REAL_DOCKER exec -e PGPASSWORD=payram123 $NAME psql -h 127.0.0.1 -U payram -d payram -Atc "$1" 2>&1; }

updater_env() {
  exec env -i PATH="$PATH" HOME="$HOME" \
      DOCKER_BIN="$E/docker" REAL_DOCKER="$REAL_DOCKER" E2E_FAIL_DIR="$E2E_FAIL_DIR" E2E_ARGV_LOG="$E2E_ARGV_LOG" \
      STATE_DIR="$E/state" BACKUP_DIR="$E/backups" \
      POLICY_URL="$E/policy.json" RUNTIME_MANIFEST_URL="$E/manifest.json" \
      IMAGE_REPO_OVERRIDE=payram-e2e/payram UPDATER_PORT=$UPORT \
      EXECUTION_MODE=execute HEALTH_CHECK_TIMEOUT_SECONDS="${HC:-600}" FETCH_TIMEOUT_SECONDS=10 "$@"
}

start_updater() {
  stop_updater
  if [ ! -f "$E/state/updater-config.json" ]; then
    ( cd "$E" && updater_env "$BIN" init --no-autoupdate >>"$E/updater.log" 2>&1 ) || fail "payram-updater init failed (see updater.log)"
  fi
  log "Starting updater (health timeout ${HC:-600}s)"
  if lsof -nP -iTCP:$UPORT -sTCP:LISTEN >/dev/null 2>&1; then fail "port $UPORT is already taken"; return 1; fi
  cd "$E" || return 1
  ( updater_env "$BIN" serve >>"$E/updater.log" 2>&1 ) &
  echo $! > "$E/updater.pid"
  cd - >/dev/null || return 1
  for _ in $(seq 1 50); do
    kill -0 "$(cat "$E/updater.pid")" 2>/dev/null || { fail "updater exited at start (see updater.log)"; return 1; }
    curl -s -m 1 "http://127.0.0.1:$UPORT/health" | grep -q ok && { pass "updater answers /health (pid $(cat "$E/updater.pid"))"; return 0; }
    sleep 0.2
  done
  fail "updater did not answer /health within 10s"
}

stop_updater() {
  [ -f "$E/updater.pid" ] || return 0
  local pid; pid=$(cat "$E/updater.pid")
  kill -TERM "$pid" 2>/dev/null && for _ in $(seq 1 900); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  rm -f "$E/updater.pid"
}

kill_updater() {
  local pid; pid=$(cat "$E/updater.pid"); kill -KILL "$pid"; rm -f "$E/updater.pid"; log "Updater killed (SIGKILL)"
}

status() { curl -s -m 5 "http://127.0.0.1:$UPORT/upgrade/status"; }
field() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get(sys.argv[1]) or "")' "$1"; }

run_update() {
  log "Update to $1"
  curl -s -m 60 -X POST "http://127.0.0.1:$UPORT/upgrade/run" -H 'Content-Type: application/json' \
    -d "{\"requestedTarget\":\"$1\"}"
  echo
}

wait_job_done() {
  local deadline=$((SECONDS + ${1:-1200})) state
  while [ $SECONDS -lt $deadline ]; do
    state=$(status | field state)
    case "$state" in READY|FAILED) break ;; esac
    sleep 3
  done
  status > "$E/last-status.json"
  python3 - "$E/last-status.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
print("  state=%s failureCode=%s causeCode=%s stage=%s" % (d.get("state"), d.get("failureCode"), d.get("causeCode"), d.get("switchStage")))
print("  message=%s" % d.get("message"))
PY
}

wait_job_state() {
  local want="$1" deadline=$((SECONDS + ${2:-600}))
  while [ $SECONDS -lt $deadline ]; do
    [ "$(status | field state)" = "$want" ] && return 0
    sleep 1
  done
  return 1
}

expect_job() {
  local state code cause
  state=$(field state < "$E/last-status.json"); code=$(field failureCode < "$E/last-status.json"); cause=$(field causeCode < "$E/last-status.json")
  [ "$state" = "$1" ] && [ "$code" = "${2:-}" ] && [ "$cause" = "${3:-}" ] \
    && pass "job $1 ${2:-} ${3:-}" || fail "job is $state/$code/$cause, want $1/${2:-}/${3:-}"
}

# The promise: one PayRam container named payram-e2e, running image, healthy,
# restart policy unless-stopped; no other payram-e2e* container running or able
# to restart on its own; the marker row survives.
expect_payram() {
  local image="payram-e2e/payram:$1"
  local got
  got=$($REAL_DOCKER inspect -f '{{.Config.Image}}|{{.State.Running}}|{{.HostConfig.RestartPolicy.Name}}' $NAME 2>/dev/null)
  [ "$got" = "$image|true|unless-stopped" ] && pass "$NAME runs $1 (restart unless-stopped)" || fail "$NAME is '$got', want $image|true|unless-stopped"
  health | grep -q '"status":"ok"' && pass "PayRam answers its health check" || fail "PayRam does not answer: $(health)"
  while read -r other; do
    [ -z "$other" ] && continue
    info=$($REAL_DOCKER inspect -f '{{.State.Running}}|{{.HostConfig.RestartPolicy.Name}}' "$other")
    case "$info" in
      false\|no) pass "$other kept stopped with restart=no" ;;
      *) fail "$other is $info (must be stopped with restart=no)" ;;
    esac
  done < <($REAL_DOCKER ps -a --format '{{.Names}}' | grep "^$NAME-")
  [ "$(sql 'select id from e2e_marker limit 1')" = "42" ] && pass "database marker row intact" || fail "marker row: $(sql 'select id from e2e_marker')"
}

expect_container() { # name exists?
  if $REAL_DOCKER inspect "$1" >/dev/null 2>&1; then [ "$2" = yes ] && pass "$1 exists" || fail "$1 exists"; else [ "$2" = no ] && pass "$1 absent" || fail "$1 absent"; fi
}

# The failed new version migrated the database and moved no money: the
# updater restores the pre-update backup, then switches back, so $OLD runs on
# the database it knows (audit H1: it used to run on the migrated one).
scenario_rollback_after_migrations() {
  fresh_payram ${OLD}${ARCH} || return
  local before; before=$(sql "select string_agg(id, ',' order by id) from migrations")
  HC=180 start_updater
  run_update 99.0.1
  wait_job_done 1500
  expect_job FAILED UPGRADE_ROLLED_BACK_RESTORED HEALTHCHECK_FAILED
  expect_payram ${OLD}${ARCH}
  expect_container $NAME-failed yes
  expect_container $NAME-previous no
  [ "$(sql "select string_agg(id, ',' order by id) from migrations")" = "$before" ] \
    && pass "the database has $OLD's migrations again (the backup was restored)" || fail "migrations differ from before the update"
  ls "$E"/backups/*pre-restore-safety* >/dev/null 2>&1 && pass "the migrated database was saved before the restore" || fail "no safety backup of the migrated database"
  grep -q "restored the backup taken at" "$E/last-status.json" && pass "message says the backup was restored" || fail "message: $(field message < "$E/last-status.json")"
}

# A build that exits at once leaves its database unreadable (the bundled
# database lives in that container), so whether it migrated cannot be told:
# the updater neither restores nor switches back, and says so.
scenario_container_exits() {
  HC=600 start_updater
  local start=$SECONDS
  run_update 99.0.2
  wait_job_done 900
  expect_job FAILED UPGRADE_DATABASE_UNCHECKED DOCKER_ERROR
  [ $((SECONDS - start)) -lt 420 ] && pass "an exiting container fails fast ($((SECONDS - start))s, not the 600s timeout)" || fail "took $((SECONDS - start))s"
  for c in $NAME $NAME-previous; do
    [ "$($REAL_DOCKER inspect -f '{{.State.Running}}' $c 2>/dev/null)" = false ] && pass "$c is stopped" || fail "$c: $($REAL_DOCKER inspect -f '{{.State.Status}}' $c 2>&1)"
  done
  [ "$($REAL_DOCKER inspect -f '{{.HostConfig.RestartPolicy.Name}}' $NAME-previous)" = no ] && pass "the previous version cannot start by itself" || fail "$NAME-previous may restart"
  fresh_payram ${OLD}${ARCH}   # the next scenarios start from a running $OLD
}

scenario_pull_fails() {
  HC=600 start_updater
  local id; id=$($REAL_DOCKER inspect -f '{{.Id}}' $NAME)
  run_update 99.0.9
  wait_job_done 300
  expect_job FAILED DOCKER_PULL_FAILED
  [ "$($REAL_DOCKER inspect -f '{{.Id}}' $NAME)" = "$id" ] && pass "PayRam's container was not touched" || fail "container changed"
  grep -q "Nothing was changed\|was not changed" "$E/last-status.json" && pass "message says nothing changed" || fail "message: $(field message < "$E/last-status.json")"
  expect_payram ${OLD}${ARCH}
}

scenario_concurrent_click() {
  HC=60 start_updater
  run_update 99.0.1 >/dev/null
  local second; second=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$UPORT/upgrade/run" -d '{"requestedTarget":"99.0.1"}')
  [ "$second" = 409 ] && pass "second Update click refused (409)" || fail "second click got $second"
  wait_job_done 900
  expect_payram ${OLD}${ARCH}
}

scenario_happy_upgrade() {
  HC=600 start_updater
  run_update "$NEW"
  wait_job_done 1500
  expect_job READY
  expect_payram ${NEW}${ARCH}
  expect_container $NAME-previous no
  ports=$($REAL_DOCKER port $NAME)
  echo "$ports" | grep -q "80/tcp -> 127.0.0.1:$HPORT" && pass "loopback-only host port kept" || fail "ports: $ports"
  echo "$ports" | grep -q "5432" && fail "database port still published: $ports" || pass "database port no longer published"
  mounts=$($REAL_DOCKER inspect -f '{{range .Mounts}}{{.Type}}:{{.Name}}->{{.Destination}} {{end}}' $NAME)
  echo "$mounts" | grep -q "volume:payram-e2e-db->/var/lib/payram/db/postgres" && pass "data volume mounted by name" || fail "mounts: $mounts"
  $REAL_DOCKER image inspect payram-e2e/payram:${OLD}${ARCH} >/dev/null 2>&1 && pass "previous image kept for a manual rollback" || fail "previous image pruned"
  ls "$E/backups" | head -3
  perms=$(python3 -c 'import os,sys; print(" ".join(sorted({oct(os.stat(f).st_mode & 0o777)[2:] for f in sys.argv[1:]})))' "$E/backups"/*)
  [ "$perms" = 600 ] && pass "backups are readable by their owner only (mode 600)" || fail "backup modes: $perms"
}

scenario_killed_mid_update() {
  HC=600 start_updater
  run_update 99.0.1
  if wait_job_state VERIFYING 900; then sleep 20; kill_updater; else fail "never reached VERIFYING"; return; fi
  $REAL_DOCKER ps -a --format '{{.Names}} {{.Image}} {{.Status}}' | grep "^$NAME"
  HC=600 start_updater
  wait_job_done 1200
  expect_job FAILED UPGRADE_ROLLED_BACK UPGRADE_INTERRUPTED
  expect_payram ${NEW}${ARCH}
}

scenario_sigterm_mid_update() {
  HC=90 start_updater
  run_update 99.0.1
  if wait_job_state VERIFYING 900; then sleep 5; else fail "never reached VERIFYING"; return; fi
  log "SIGTERM to the updater during verification"
  local pid; pid=$(cat "$E/updater.pid"); kill -TERM "$pid"
  for _ in $(seq 1 900); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  rm -f "$E/updater.pid"
  grep -q "Waiting up to" "$E/updater.log" && pass "updater waited for the update before exiting" || fail "updater did not wait"
  python3 - "$E/state/jobs/latest/status.json" <<'PY'
import json,sys; d=json.load(open(sys.argv[1])); print("  job after shutdown: state=%s code=%s cause=%s" % (d["state"], d.get("failureCode"), d.get("causeCode")))
PY
  [ "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["state"])' "$E/state/jobs/latest/status.json")" = FAILED ] && pass "job finished (switched back) before the updater exited" || fail "job left active"
  expect_payram ${NEW}${ARCH}
}

scenario_start_fails() {
  HC=600 start_updater
  touch "$E2E_FAIL_DIR/start-$NAME"
  run_update "$NEW"
  wait_job_done 900
  expect_job FAILED UPGRADE_ROLLED_BACK DOCKER_ERROR
  expect_payram ${NEW}${ARCH}
}

scenario_manual_mode_token() {
  HC=600 start_updater
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$UPORT/upgrade/plan" -H 'Content-Type: application/json' \
    -d "{\"mode\":\"MANUAL\",\"source\":\"CLI\",\"requestedTarget\":\"$NEW\"}")
  [ "$code" = 403 ] && pass "manual mode refused to a program without root's token (403)" || fail "manual mode without the token got $code"
  cli dry-run --mode manual --to "$NEW" > "$E/dry-run.log" 2>&1 && grep -q '"mode": "MANUAL"' "$E/dry-run.log" \
    && pass "payram-updater dry-run --mode manual works with the token" || fail "CLI manual dry-run: $(tail -3 "$E/dry-run.log")"
  [ "$(python3 -c 'import os,sys; print(oct(os.stat(sys.argv[1]).st_mode & 0o777)[2:])' "$E/state/cli-token")" = 600 ] \
    && pass "CLI token is readable by its owner only" || fail "cli-token mode"
}

scenario_backup_fails() {
  HC=600 start_updater
  # The dump itself fails (all 3 attempts), after PayRam's programs were stopped.
  echo 3 > "$E2E_FAIL_DIR/arg--Fc"
  run_update "$NEW"
  wait_job_done 900
  rm -f "$E2E_FAIL_DIR/arg--Fc"
  expect_job FAILED BACKUP_FAILED_AFTER_QUIESCE
  grep -q "was not changed" "$E/last-status.json" && pass "message says PayRam was not changed" || fail "message: $(field message < "$E/last-status.json")"
  local stopped; stopped=$($REAL_DOCKER exec $NAME supervisorctl status 2>/dev/null | grep -v -E 'postgres|redis' | grep -c -E 'STOPPED|EXITED')
  [ "$stopped" = 0 ] && pass "every program stopped for the backup runs again" || fail "$stopped programs still stopped: $($REAL_DOCKER exec $NAME supervisorctl status | grep -E 'STOPPED|EXITED')"
  expect_container $NAME-next no
  expect_payram ${OLD}${ARCH}
}

# A job left active by an updater from before the container switch (v0.2.3):
# it stopped PayRam's programs for the backup and died. The new updater, on
# start, starts them again and ends the job, and the next update works.
scenario_interrupted_old_updater_job() {
  fresh_payram ${OLD}${ARCH} || return
  HC=600 start_updater
  stop_updater
  $REAL_DOCKER exec $NAME supervisorctl stop payram-core webhook-processor deposit-processor >/dev/null 2>&1
  mkdir -p "$E/state/jobs/latest"
  cat > "$E/state/jobs/latest/status.json" <<JSON
{"jobId":"job-v023","mode":"DASHBOARD","requestedTarget":"$NEW","resolvedTarget":"$NEW","state":"BACKING_UP","failureCode":"","message":"Creating database backup","createdAt":"2026-10-20T10:00:00Z","updatedAt":"2026-10-20T10:01:00Z"}
JSON
  HC=600 start_updater
  wait_job_done 900
  expect_job FAILED UPGRADE_INTERRUPTED
  local stopped; stopped=$($REAL_DOCKER exec $NAME supervisorctl status 2>/dev/null | grep -c -E '^(payram-core|webhook-processor|deposit-processor) .*(STOPPED|EXITED)')
  [ "$stopped" = 0 ] && pass "programs the old updater stopped run again" || fail "programs still stopped"
  expect_payram ${OLD}${ARCH}
  run_update "$NEW"
  wait_job_done 1500
  expect_job READY
  expect_payram ${NEW}${ARCH}
}

# PayRam set up the way setup_payram.sh does it on a server with a
# certificate. The update must keep every host folder (the certificate one
# read-only), both web ports and every setting, an empty one included.
scenario_setup_script_install() {
  fresh_payram ${OLD}${ARCH} setup || return
  HC=600 start_updater
  run_update "$NEW"
  wait_job_done 1500
  expect_job READY
  expect_payram ${NEW}${ARCH}
  local mounts env ports
  mounts=$($REAL_DOCKER inspect -f '{{range .Mounts}}{{.Type}}:{{.Source}}->{{.Destination}}:{{.RW}} {{end}}' $NAME)
  echo "$mounts" | grep -q "bind:[^ ]*/host/core->/root/payram:true" && pass "host folder for /root/payram kept" || fail "mounts: $mounts"
  echo "$mounts" | grep -q "bind:[^ ]*/certs->/etc/letsencrypt:false" && pass "certificate folder kept, read-only" || fail "mounts: $mounts"
  env=$($REAL_DOCKER inspect -f '{{range .Config.Env}}{{println .}}{{end}}' $NAME)
  echo "$env" | grep -qx "AES_KEY=$AES" && pass "AES key carried over unchanged" || fail "AES_KEY not carried over"
  echo "$env" | grep -qx "SSL_CERT_PATH=/etc/letsencrypt" && pass "SSL_CERT_PATH kept" || fail "SSL_CERT_PATH: $(echo "$env" | grep SSL_CERT_PATH)"
  echo "$env" | grep -qx "PAYRAM_E2E_EMPTY=" && pass "a setting with an empty value is kept, empty" || fail "PAYRAM_E2E_EMPTY: '$(echo "$env" | grep PAYRAM_E2E_EMPTY)'"
  ports=$($REAL_DOCKER port $NAME)
  echo "$ports" | grep -q "443/tcp -> 127.0.0.1:$SPORT" && echo "$ports" | grep -q "80/tcp -> 127.0.0.1:$HPORT" \
    && pass "both web ports kept" || fail "ports: $ports"
  echo "$ports" | grep -q 5432 && fail "database port still published" || pass "database port no longer published"
}

# PayRam answers on HTTPS only (443), the case of the merchant whose update
# was reported failed: the updater must find it there and check it over TLS.
scenario_https_only_install() {
  fresh_payram ${OLD}${ARCH} https || return
  HC=600 start_updater
  run_update "$NEW"
  wait_job_done 1500
  expect_job READY
  grep -q "PayRam answers at https://127.0.0.1:$SPORT" "$E/state/jobs/latest/logs.txt" \
    && pass "updater found PayRam on https://127.0.0.1:$SPORT" || fail "log: $(grep -i 'answers at\|CORE_BASE_URL\|not answering' "$E/state/jobs/latest/logs.txt" | tail -2)"
  expect_payram ${NEW}${ARCH}
}

# A container started with --rm is deleted by Docker when it stops, so the
# updater could never switch back: it must refuse before touching anything.
scenario_auto_remove_refused() {
  fresh_payram ${OLD}${ARCH} autoremove || return
  HC=600 start_updater
  local id; id=$($REAL_DOCKER inspect -f '{{.Id}}' $NAME)
  run_update "$NEW"
  wait_job_done 300
  expect_job FAILED CONTAINER_NOT_REPLACEABLE
  [ "$($REAL_DOCKER inspect -f '{{.Id}} {{.State.Running}}' $NAME 2>/dev/null)" = "$id true" ] \
    && pass "PayRam's container was not touched and still runs" || fail "container changed or stopped"
  health | grep -q '"status":"ok"' && pass "PayRam answers its health check" || fail "PayRam does not answer"
  expect_container $NAME-next no
  $REAL_DOCKER rm -f $NAME >/dev/null 2>&1
}

# The failed new version migrated the database AND started a sweep (a lock
# row PayRam writes before it broadcasts one) while it ran: restoring would
# forget it, so the updater neither restores nor switches back. PayRam stays
# on the new version; the old one stays aside, stopped.
scenario_money_moved_not_restored() {
  fresh_payram ${OLD}${ARCH} || return
  HC=120 start_updater
  run_update 99.0.1
  if wait_job_state VERIFYING 900; then sleep 20; else fail "never reached VERIFYING"; return; fi
  sql "insert into generic_data_stores (created_at, updated_at, type, valid_till, data, status) values (now(), now(), 'sweep_initiated', now() + interval '9 hours', '{\"address\":\"0xe2e\",\"txHash\":\"0xe2e\"}', 'active')" >/dev/null \
    && pass "a sweep was started by the new version (simulated)" || fail "could not insert the sweep lock: $(sql 'select 1')"
  wait_job_done 1500
  expect_job FAILED UPGRADE_MONEY_MOVED_NOT_RESTORED HEALTHCHECK_FAILED
  [ "$($REAL_DOCKER inspect -f '{{.Config.Image}}|{{.State.Running}}' $NAME)" = "payram-e2e/payram:99.0.1${ARCH}|true" ] \
    && pass "$NAME was left on the new version, running" || fail "$NAME: $($REAL_DOCKER inspect -f '{{.Config.Image}}|{{.State.Status}}' $NAME)"
  [ "$($REAL_DOCKER inspect -f '{{.State.Running}}|{{.HostConfig.RestartPolicy.Name}}' $NAME-previous)" = "false|no" ] \
    && pass "$NAME-previous kept stopped with restart=no" || fail "$NAME-previous: $($REAL_DOCKER inspect -f '{{.State.Status}}|{{.HostConfig.RestartPolicy.Name}}' $NAME-previous)"
  [ "$(sql "select count(*) from generic_data_stores where data like '%0xe2e%'")" = 1 ] && pass "the sweep record was not restored away" || fail "sweep record gone"
  [ "$(sql 'select id from e2e_marker limit 1')" = "42" ] && pass "database marker row intact" || fail "marker row"
  local stopped; stopped=$($REAL_DOCKER exec $NAME supervisorctl status 2>/dev/null | grep -v -E 'postgres|redis' | grep -c -E 'STOPPED')
  [ "$stopped" = 0 ] && pass "the new version's programs run again (left as it was)" || fail "$stopped programs still stopped"
  grep -q "Do not restore the backup" "$E/last-status.json" && pass "message warns against restoring" || fail "message: $(field message < "$E/last-status.json")"
  $REAL_DOCKER rm -f $NAME $NAME-previous >/dev/null 2>&1
}

# Over the whole run, no docker command the updater ran carried the AES key or
# the database password: anyone on the server can read a command line.
check_no_secrets_on_command_lines() {
  log "=== no secrets on docker command lines ==="
  local calls; calls=$(wc -l < "$E2E_ARGV_LOG" | tr -d ' ')
  if grep -q -e "$AES" -e payram123 "$E2E_ARGV_LOG"; then
    fail "a secret reached a docker command line: $(grep -e "$AES" -e payram123 "$E2E_ARGV_LOG" | head -2 | cut -c1-200)"
  else
    pass "none of the updater's $calls docker commands carried the AES key or the database password"
  fi
}

ensure_test_images() {
  for v in "$OLD" "$NEW"; do
    $REAL_DOCKER image inspect "payramapp/payram:${v}${ARCH}" >/dev/null 2>&1 || $REAL_DOCKER pull -q "payramapp/payram:${v}${ARCH}" >/dev/null
  done
  $REAL_DOCKER image inspect payram-e2e/payram:${OLD}${ARCH} >/dev/null 2>&1 || $REAL_DOCKER tag payramapp/payram:${OLD}${ARCH} payram-e2e/payram:${OLD}${ARCH}
  $REAL_DOCKER image inspect payram-e2e/payram:${NEW}${ARCH} >/dev/null 2>&1 || $REAL_DOCKER tag payramapp/payram:${NEW}${ARCH} payram-e2e/payram:${NEW}${ARCH}
  # Built from the new release: 99.0.1 runs its migrations but the API is
  # unreachable through nginx; 99.0.2 exits as soon as it starts.
  $REAL_DOCKER image inspect payram-e2e/payram:99.0.1${ARCH} >/dev/null 2>&1 || printf '%s\n' "FROM payramapp/payram:${NEW}${ARCH}" 'RUN sed -i "s#location /api/v1/ {#location /api/v1-disabled/ {#" /etc/nginx/payram-locations.conf' | $REAL_DOCKER build -q -t payram-e2e/payram:99.0.1${ARCH} - >/dev/null
  $REAL_DOCKER image inspect payram-e2e/payram:99.0.2${ARCH} >/dev/null 2>&1 || printf '%s\n' "FROM payramapp/payram:${NEW}${ARCH}" 'ENTRYPOINT ["sh", "-c", "echo broken build; exit 3"]' | $REAL_DOCKER build -q -t payram-e2e/payram:99.0.2${ARCH} - >/dev/null
}

cli() { ( cd "$E" && updater_env "$BIN" "$@" ); }

scenario_db_only_restore() {
  local backup; backup=$(ls -t "$E"/backups/payram-backup-*-${OLD}-to-${NEW}*.dump 2>/dev/null | head -1)
  [ -n "$backup" ] || { fail "no pre-upgrade backup to restore"; return; }
  sql "insert into e2e_marker values (99);" >/dev/null
  log "Database-only restore of $(basename "$backup") into the running $NEW"
  cli backup restore --file "$backup" --mode db-only --yes > "$E/restore.log" 2>&1 && pass "restore command succeeded" || { fail "restore failed: $(tail -5 "$E/restore.log")"; return; }
  [ -z "$(sql 'select id from e2e_marker where id = 99')" ] && pass "row written after the backup is gone (the restore really restored)" || fail "marker 99 still there"
  wait_payram_healthy 600 && pass "PayRam healthy after the restore" || fail "PayRam not healthy after restore"
  expect_payram ${NEW}${ARCH}
  ls "$E"/backups/*pre-restore-safety* >/dev/null 2>&1 && pass "safety backup of the replaced database exists" || fail "no safety backup"
}

scenario_full_recovery() {
  local backup; backup=$(ls -t "$E"/backups/payram-backup-*-${OLD}-to-${NEW}*.dump 2>/dev/null | head -1)
  [ -n "$backup" ] || { fail "no pre-upgrade backup to restore"; return; }
  sql "insert into e2e_marker values (77);" >/dev/null
  log "Full recovery to $OLD with $(basename "$backup")"
  cli backup restore --file "$backup" --mode full --yes > "$E/restore.log" 2>&1 && pass "full recovery command succeeded" || { fail "full recovery failed: $(tail -8 "$E/restore.log")"; return; }
  wait_payram_healthy 600 && pass "PayRam healthy after full recovery" || fail "PayRam not healthy"
  expect_payram ${OLD}${ARCH}
  [ -z "$(sql 'select id from e2e_marker where id = 77')" ] && pass "database is the pre-update backup" || fail "marker 77 still there"
  echo "  migrations now: $(sql 'select count(*) from migrations')"
  expect_container $NAME-previous no
}

write_config
ensure_test_images
[ $# -eq 0 ] && set -- rollback_after_migrations container_exits pull_fails concurrent_click manual_mode_token backup_fails \
  happy_upgrade db_only_restore killed_mid_update sigterm_mid_update start_fails full_recovery \
  interrupted_old_updater_job setup_script_install https_only_install auto_remove_refused \
  money_moved_not_restored
for s in "$@"; do
  ensure_test_images
  log "=== $s ==="
  "scenario_$s"
done
stop_updater
check_no_secrets_on_command_lines
log "Done: $FAILURES failure(s)"
exit $FAILURES
