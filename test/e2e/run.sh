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
REAL_DOCKER="${REAL_DOCKER:-$(command -v docker)}"
export REAL_DOCKER
OLD="${E2E_OLD:-3.8.1}"
NEW="${E2E_NEW:-3.10.0}"
case "$(uname -m)" in arm64|aarch64) ARCH="-arm64" ;; *) ARCH="" ;; esac
export E2E_FAIL_DIR="$E/fail"
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

health() { curl -s -m 3 "http://127.0.0.1:$HPORT/api/v1/health"; }

wait_payram_healthy() {
  local deadline=$((SECONDS + ${1:-600}))
  while [ $SECONDS -lt $deadline ]; do
    health | grep -q '"status":"ok"' && return 0
    sleep 3
  done
  return 1
}

fresh_payram() {
  local version="$1"
  log "Fresh PayRam $version"
  $REAL_DOCKER rm -f $NAME $NAME-next $NAME-previous $NAME-failed >/dev/null 2>&1
  $REAL_DOCKER ps -a --format '{{.Names}}' | grep "^$NAME-previous-" | xargs -r $REAL_DOCKER rm -f >/dev/null 2>&1
  $REAL_DOCKER volume rm payram-e2e-core payram-e2e-log payram-e2e-db >/dev/null 2>&1
  rm -rf "$E/state" "$E/backups"; mkdir -p "$E/state" "$E/backups"
  $REAL_DOCKER run -d --name $NAME --restart unless-stopped \
    -p 127.0.0.1:$HPORT:80 -p 127.0.0.1:15432:5432 \
    -e AES_KEY=6b1f0e1d2c3b4a5968778695a4b3c2d1e0f1a2b3c4d5e6f708192a3b4c5d6e7f \
    -e BLOCKCHAIN_NETWORK_TYPE=testnet -e SERVER=DEVELOPMENT -e POSTGRES_SSLMODE=disable \
    -e POSTGRES_HOST=localhost -e POSTGRES_PORT=5432 -e POSTGRES_DATABASE=payram \
    -e POSTGRES_USERNAME=payram -e POSTGRES_PASSWORD=payram123 -e SSL_CERT_PATH= \
    -e PAYMENTS_APP_SERVER_URL=https://x.payram.com \
    -v payram-e2e-core:/root/payram -v payram-e2e-log:/var/log -v payram-e2e-db:/var/lib/payram/db/postgres \
    payram-e2e/payram:$version >/dev/null || { fail "could not start PayRam $version"; return 1; }
  if wait_payram_healthy 600; then pass "PayRam $version healthy"; else fail "PayRam $version never healthy"; return 1; fi
  sql "create table if not exists e2e_marker(id int); insert into e2e_marker values (42);" >/dev/null
}

sql() { $REAL_DOCKER exec -e PGPASSWORD=payram123 $NAME psql -h 127.0.0.1 -U payram -d payram -Atc "$1" 2>&1; }

updater_env() {
  exec env -i PATH="$PATH" HOME="$HOME" \
      DOCKER_BIN="$E/docker" REAL_DOCKER="$REAL_DOCKER" E2E_FAIL_DIR="$E2E_FAIL_DIR" \
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
  cd "$E"
  ( updater_env "$BIN" serve >>"$E/updater.log" 2>&1 ) &
  echo $! > "$E/updater.pid"
  cd - >/dev/null
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
  local got running restart
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

scenario_rollback_after_migrations() {
  fresh_payram ${OLD}${ARCH} || return
  local before; before=$(sql "select count(*) from migrations" )
  HC=180 start_updater
  run_update 99.0.1
  wait_job_done 1500
  expect_job FAILED UPGRADE_ROLLED_BACK HEALTHCHECK_FAILED
  expect_payram ${OLD}${ARCH}
  expect_container $NAME-failed yes
  expect_container $NAME-previous no
  echo "  migrations before=$before after=$(sql 'select count(*) from migrations') (the failed new version may have migrated; $OLD must still run)"
}

scenario_container_exits() {
  HC=600 start_updater
  local start=$SECONDS
  run_update 99.0.2
  wait_job_done 900
  expect_job FAILED UPGRADE_ROLLED_BACK DOCKER_ERROR
  [ $((SECONDS - start)) -lt 420 ] && pass "an exiting container fails fast ($((SECONDS - start))s, not the 600s timeout)" || fail "took $((SECONDS - start))s"
  expect_payram ${OLD}${ARCH}
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
  HC=600 start_updater
  run_update 99.0.2 >/dev/null
  local second; second=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$UPORT/upgrade/run" -d '{"requestedTarget":"99.0.2"}')
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
[ $# -eq 0 ] && set -- rollback_after_migrations container_exits pull_fails concurrent_click happy_upgrade db_only_restore killed_mid_update sigterm_mid_update start_fails full_recovery
for s in "$@"; do
  ensure_test_images
  log "=== $s ==="
  "scenario_$s"
done
stop_updater
log "Done: $FAILURES failure(s)"
exit $FAILURES
