#!/bin/sh
# E2E docker stand-in: real docker, except that pulling an image that exists
# only locally (the test-only broken versions) counts as already pulled, and
# failures can be injected with files in $E2E_FAIL_DIR named after the call
# ("start-payram-e2e" fails the next `docker start payram-e2e` once).
if [ -n "$E2E_FAIL_DIR" ] && [ -n "$2" ]; then
  key="$1-$(printf '%s' "$2" | tr '/:' '__')"
  if [ -f "$E2E_FAIL_DIR/$key" ]; then rm -f "$E2E_FAIL_DIR/$key"; echo "injected failure: docker $*" >&2; exit 1; fi
fi
if [ "$1" = pull ] && "$REAL_DOCKER" image inspect "$2" >/dev/null 2>&1; then
  echo "Status: Image is up to date for $2"; exit 0
fi
exec "$REAL_DOCKER" "$@"
