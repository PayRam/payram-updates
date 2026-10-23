# PayRam Updater

Manages PayRam runtime upgrades with automated rollback and recovery capabilities.

## Quick Start

Download and run the installation script:

```bash
curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo bash
```

Checksum verification (required): the installer verifies the downloaded binary using a published checksum. You can also pass a checksum explicitly:

```bash
PAYRAM_UPDATER_SHA256="<sha256>" curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo -E bash
```

Verify the service is running:

```bash
sudo systemctl status payram-updater
payram-updater status
```

## What It Does

The PayRam Updater is a background service that:
- Manages PayRam container upgrades safely
- Creates automatic database backups before upgrades
- Provides health monitoring and recovery tools
- Prevents problematic upgrades through policy enforcement
- Maintains upgrade history and logs

## Basic Commands

### Check upgrade status
```bash
payram-updater status
```

### Check service health  
```bash
payram-updater health
```

Returns:
```json
{"status":"ok"}
```

### View upgrade logs
```bash
payram-updater logs
```

### Follow logs in real-time
```bash
payram-updater logs -f
```

### Restart the service
```bash
payram-updater restart
```

Restarts the `payram-updater` systemd service. Use this when:
- The service started before Docker (container not discovered)
- Configuration changes require a reload
- Service needs to re-scan for containers

Requires `sudo` access and systemd.

## Performing Upgrades

### Validate an upgrade (dry-run)
```bash
payram-updater dry-run --to latest
```

### Execute an upgrade

Upgrade to the latest version (manual mode):
```bash
payram-updater run --to latest
```

Dashboard-controlled upgrade:
```bash
payram-updater run --mode dashboard --to latest
```

You'll see a confirmation prompt before the upgrade starts:
```
╔══════════════════════════════════════════════════════════════╗
║                     UPGRADE SUMMARY                          ║
╠══════════════════════════════════════════════════════════════╣
║  Mode:             DASHBOARD                                 ║
║  Requested Target: latest                                    ║
║  Image:            payramapp/payram                          ║
║  Container:        payram                                    ║
╠══════════════════════════════════════════════════════════════╣
║  ⚠️  This will stop and replace the container.               ║
║     Brief downtime expected.                                 ║
╚══════════════════════════════════════════════════════════════╝

Proceed? (y/N):
```

### Skip confirmation (for automation)
```bash
payram-updater run --to 1.7.8 --yes
```

### Upgrade to a specific version
```bash
payram-updater run --to 1.7.8
```

## Upgrade Modes

**Manual Mode** (default)
- Allows upgrades to any version
- Bypasses policy breakpoints
- Can use "latest" to get newest version from policy
- For operator-initiated upgrades
- Use when you need to override policy restrictions

**Dashboard Mode** (recommended for automated upgrades)
- Uses policy-controlled version selection
- Blocks upgrades that require manual intervention
- Resolves "latest" from the upgrade policy
- Safer for automated systems
- Enable with `--mode dashboard`

## Recovery & Troubleshooting

An update never deletes payment data, and whatever fails, the updater's aim is
that PayRam ends up running. The new version is started next to the running
one; if it fails its checks, the updater switches back to the previous version
on its own. Before anything else after a failed update, check whether PayRam
answers:

```bash
curl -sk https://127.0.0.1/api/v1/health   # HTTPS installs
curl -s http://127.0.0.1/api/v1/health     # HTTP-only installs
```

A reply containing `"status":"ok"` means PayRam is running: nothing needs
restoring.

### Diagnose system health
```bash
payram-updater inspect
```

This shows:
- Current system state (OK, DEGRADED, or BROKEN)
- Detected issues and their severity
- Recovery recommendations

### Check what needs recovering
```bash
payram-updater recover
```

Reads the last update and tells you what, if anything, to do. It only reads:
it never stops, removes or restarts a container. When PayRam was not changed
or was already put back, it confirms PayRam's container is running and says
so; when a person has to look, it prints the recovery steps.

### View recovery guidance
```bash
curl http://127.0.0.1:2567/upgrade/playbook
```

Shows detailed recovery steps for the current failure. Every playbook starts
with read-only checks. Restoring a backup is only ever offered as the last
step, to do only if PayRam support asks you to: it removes every payment,
deposit and payout recorded after the backup was taken.

### What a failed update means

| Code | Is PayRam running? | What to do |
|------|--------------------|------------|
| `UPGRADE_ROLLED_BACK` | Yes, on the version it had before. The new version failed (the cause is shown) and the updater switched back. The database was not restored, so payments recorded during the update are kept. | Nothing to recover. Send the updater logs to PayRam support, then retry later. |
| `ROLLBACK_FAILED` | Maybe not. The new version failed and switching back failed too. | Follow the playbook: it shows how to start `<name>-previous`, the version that ran before. Do not restore a backup unless support asks. |
| `UPGRADE_INTERRUPTED` | Yes. The updater stopped mid-update (restart, crash, reboot) and put PayRam back when it came up. | Retry the update. |
| `DOCKER_CREATE_FAILED`, `TARGET_NOT_ALLOWED`, `DOCKER_PULL_FAILED`, `BACKUP_FAILED`, `BACKUP_TIMEOUT`, `DISK_SPACE_LOW`, `POLICY_FETCH_FAILED`, `MANIFEST_FETCH_FAILED`, ... | Yes, unchanged: the update stopped before PayRam was touched. | Fix the cause shown in the playbook and retry. |
| `HEALTHCHECK_FAILED`, `VERSION_MISMATCH`, `DOCKER_ERROR` (from older updater versions) | Often yes: check first. Updater v0.2.3 reports `HEALTHCHECK_FAILED` on every install that publishes only ports 80/443, even when PayRam is healthy. | Run the health check above. If it replies ok, run `payram-updater sync` and you are done. |

During an update the updater saves its progress at least every 30 seconds. If
`payram-updater status` shows an update in progress whose `updatedAt` has not
changed for more than 5 minutes, the updater has stopped working on it: restart
it with `sudo systemctl restart payram-updater`. When it starts, it puts PayRam
back on its own. `payram-updater sync` refuses to run while an update is in
progress or interrupted.

### Freeing disk space safely

The updater keeps the previous PayRam container and its image so it can switch
back. To free space, use `docker image prune` (untagged layers only) and
`docker builder prune`, and check `sudo du -sh /var/lib/payram/backups
/var/log/journal`. Do not run `docker system prune`, `docker container prune`
or any prune with `-a`: they delete what a switch-back needs.

### Database commands

PayRam's bundled database is never published on the host. Run database
commands inside the container, for example:

```bash
docker exec payram sh -c 'pg_isready -U "${POSTGRES_USER:-${POSTGRES_USERNAME:-payram}}" -d "${POSTGRES_DB:-${POSTGRES_DATABASE:-payram}}"'
```

## Database Backups

Backups are automatically created before each upgrade.

### List available backups
```bash
payram-updater backup list
```

### Create a manual backup
```bash
payram-updater backup create
```

### Restore from a backup
```bash
payram-updater backup restore --file /path/to/backup.dump
```

⚠️ **Warning**: Restore replaces all current database data with the backup contents. You'll be prompted for confirmation unless you use `--yes`.

## Configuration

The service is configured via environment variables in `/etc/payram/updater.env`.

### Core Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `UPDATER_PORT` | `2567` | HTTP API port |
| `POLICY_URL` | Required | Upgrade policy JSON URL |
| `RUNTIME_MANIFEST_URL` | Required | Container manifest JSON URL |
| `STATE_DIR` | `/var/lib/payram-updater` | Job state persistence directory |
| `FETCH_TIMEOUT_SECONDS` | `10` | HTTP request timeout |
| `DOCKER_BIN` | `docker` | Docker binary path |

### Database Backup Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `BACKUP_DIR` | `data/backups` | Backup storage directory |
| `BACKUP_RETENTION` | `10` | Number of backups to keep |
| `PG_HOST` | `127.0.0.1` | PostgreSQL host |
| `PG_PORT` | `5432` | PostgreSQL port |
| `PG_DB` | `payram` | Database name |
| `PG_USER` | `payram` | Database user |
| `PG_PASSWORD` | (empty) | Database password |

### Advanced Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `DEBUG_VERSION_MODE` | `false` | Allow arbitrary version strings (testing) |
| `IMAGE_REPO_OVERRIDE` | (none) | Override image repository for testing |
| `TARGET_CONTAINER_NAME` | (auto-detect) | Override target container name |

To reconfigure:
```bash
sudo nano /etc/payram/updater.env
sudo systemctl restart payram-updater
```

See `packaging/examples/updater.env.example` for a complete configuration template.

## View Service Logs

```bash
sudo journalctl -u payram-updater -f
```

## HTTP API

The service provides an HTTP API on port `2567` (default, configurable via `UPDATER_PORT`). 

**Security:** API access is restricted to:
- Localhost (`127.0.0.1`, `::1`)
- PayRam container IP (auto-discovered)

Other Docker containers are blocked. The API is primarily used by the PayRam dashboard for orchestrating upgrades.

### Key Endpoints

**Health check**
```bash
curl http://127.0.0.1:2567/health
# Returns: {"status":"ok"}
```

**Get upgrade status**
```bash
curl http://127.0.0.1:2567/upgrade/status
```

**Get upgrade logs**
```bash
curl http://127.0.0.1:2567/upgrade/logs
```

**View upgrade history**
```bash
curl http://127.0.0.1:2567/history
```

**System diagnostics**
```bash
curl http://127.0.0.1:2567/upgrade/inspect
```

### Two-Phase Upgrade Flow (API)

The dashboard uses a two-phase approach:

**1. Plan (validation)**
```bash
curl -X POST http://127.0.0.1:2567/upgrade/plan \
  -H "Content-Type: application/json" \
  -d '{"mode":"dashboard","requestedTarget":"1.7.8"}'
```

Validates the upgrade without executing. Returns resolved version and any blocking issues.

**2. Run (execution)**
```bash
curl -X POST http://127.0.0.1:2567/upgrade/run \
  -H "Content-Type: application/json" \
  -d '{"mode":"dashboard","requestedTarget":"1.7.8"}'
```

Executes the upgrade. Returns job ID for status tracking.

**Note:** API endpoints always use `DASHBOARD` mode (strict policy enforcement). Use CLI for `MANUAL` mode upgrades.

For complete API documentation, see [API.md](API.md).

## Uninstall

```bash
sudo systemctl stop payram-updater
sudo systemctl disable payram-updater
sudo rm /etc/systemd/system/payram-updater.service
sudo rm /usr/local/bin/payram-updater
sudo rm -rf /etc/payram /var/lib/payram-updater /var/lib/payram /var/log/payram-updater
sudo systemctl daemon-reload
```

## Support

For issues or questions:
- Check logs: `sudo journalctl -u payram-updater -f`
- Run diagnostics: `payram-updater inspect`
- Contact PayRam support: Telegram [t.me/PayRamChat](https://t.me/PayRamChat) or [support@payram.com](mailto:support@payram.com). Attach `payram-updater status`, `payram-updater logs` and `sudo journalctl -u payram-updater --since "6 hours ago" --no-pager`.
