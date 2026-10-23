# End-to-end tests

`run.sh` runs the updater built from this checkout against real PayRam
containers (released images from Docker Hub) and checks, after every
scenario, the one promise an update makes to a merchant:

> exactly one PayRam container is running and answering its health check,
> with its original restart policy; no other PayRam container can start on
> its own; the database's marker row is intact.

```bash
test/e2e/run.sh                      # every scenario, in order (about an hour)
test/e2e/run.sh happy_upgrade        # one or more scenarios by name
E2E_OLD=3.9.0 E2E_NEW=3.10.0 test/e2e/run.sh
```

Needs Docker, Go, curl, python3, lsof and about 8 GB of disk. It works on
macOS (Docker Desktop) and Linux, on amd64 and arm64 (it picks the `-arm64`
images on arm64). Run it before every updater release, and whenever the
update flow (`internal/switchover`, `internal/http/upgrade_*.go`) changes.

## What it runs

| Scenario | Starts on | Does | Expects |
|---|---|---|---|
| `rollback_after_migrations` | fresh `E2E_OLD` | update to a build that migrates the database, then never answers | switched back (`UPGRADE_ROLLED_BACK` / `HEALTHCHECK_FAILED`); the old version runs on the migrated database |
| `container_exits` | old | update to a build that exits at once | switched back well before the health timeout |
| `pull_fails` | old | update to a version that cannot be downloaded | `DOCKER_PULL_FAILED`; the container was not touched |
| `concurrent_click` | old | two Update requests at once | the second is refused (409) |
| `happy_upgrade` | old | update to `E2E_NEW` | `READY`; ports (loopback binding kept, database port dropped), named volumes, previous image kept, backups mode 600 |
| `db_only_restore` | new | `backup restore --mode db-only` of the pre-update backup | rows written after the backup are gone; PayRam healthy; safety backup taken |
| `killed_mid_update` | new | SIGKILL the updater during the health check, start it again | the startup repair switches back |
| `sigterm_mid_update` | new | SIGTERM the updater during the health check | it finishes (switches back) before exiting |
| `start_fails` | new | the new container fails to start (injected) | switched back (`DOCKER_ERROR`) |
| `full_recovery` | new | `backup restore --mode full` to the old version | old version running on the pre-update database |

## How it stays out of the way

- PayRam runs as container `payram-e2e` on `127.0.0.1:18080` with named
  volumes `payram-e2e-*`; the updater listens on `127.0.0.1:25670`. The
  script refuses to start if that port is taken.
- Images are tagged under the test-only repository `payram-e2e/payram`
  (`IMAGE_REPO_OVERRIDE`), so pruning after an update never touches
  `payramapp/payram` tags or any other image. Versions `99.0.x` are the
  deliberately broken test builds.
- `docker-shim.sh` stands in for `docker`: a `pull` of an image that exists
  only locally counts as pulled, and a file `$E2E_WORK_DIR/fail/<cmd>-<name>`
  makes the next matching docker call fail once (failure injection).
- State, backups and logs live in `E2E_WORK_DIR` (default
  `$TMPDIR/payram-updater-e2e`); `updater.log` there has the updater's own
  output.

Clean up afterwards with
`docker rm -f payram-e2e payram-e2e-failed; docker volume rm payram-e2e-core payram-e2e-log payram-e2e-db; docker rmi $(docker images -q payram-e2e/payram)`.
