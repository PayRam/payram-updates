#!/bin/sh
# E2E docker stand-in: real docker, except that
#   - pulling an image that exists only locally (the test-only broken
#     versions) counts as already pulled;
#   - failures can be injected with files in $E2E_FAIL_DIR: "<cmd>-<arg>"
#     ("start-payram-e2e" fails the next `docker start payram-e2e`), or
#     "arg-<word>" (fails the next call that has the argument <word>, such as
#     "arg--Fc" for the database dump). The file may hold a count (default 1);
#   - every call's arguments are appended to $E2E_ARGV_LOG, so the test can
#     check that no secret ever reached a docker command line.
if [ -n "$E2E_ARGV_LOG" ]; then
  printf '%s\n' "$*" >> "$E2E_ARGV_LOG"
fi

# fail_if_injected KEY ARGS...: fail this call when $E2E_FAIL_DIR/KEY exists.
fail_if_injected() {
  file="$E2E_FAIL_DIR/$1"
  shift
  [ -f "$file" ] || return 0
  count=$(cat "$file" 2>/dev/null)
  case "$count" in ''|*[!0-9]*) count=1 ;; esac
  if [ "$count" -gt 1 ]; then echo $((count - 1)) > "$file"; else rm -f "$file"; fi
  echo "injected failure: docker $*" >&2
  exit 1
}

if [ -n "$E2E_FAIL_DIR" ] && [ -n "$2" ]; then
  fail_if_injected "$1-$(printf '%s' "$2" | tr '/:' '__')" "$@"
  for arg in "$@"; do
    fail_if_injected "arg-$(printf '%s' "$arg" | tr '/:' '__')" "$@"
  done
fi
if [ "$1" = pull ] && "$REAL_DOCKER" image inspect "$2" >/dev/null 2>&1; then
  echo "Status: Image is up to date for $2"; exit 0
fi
exec "$REAL_DOCKER" "$@"
