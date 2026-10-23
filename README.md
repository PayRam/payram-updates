# PayRam Updater

Manages PayRam runtime upgrades with automated rollback and recovery capabilities.

## Quick Start

Install the updater, or upgrade an existing one, with one command:

```bash
curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo bash
```

Verify the service is running:

```bash
sudo systemctl status payram-updater
payram-updater version
payram-updater status
```

### Upgrading the updater

Run the same command again. It is safe to re-run at any time:

- **Your settings are kept.** Settings already in `/etc/payram/updater.env` are never changed or removed. Settings the file does not have yet are added with their defaults, after a copy of the file is saved to `/etc/payram/installer-backups/`. If you pass a value (for example `UPDATER_PORT=3000`) that the file already sets differently, the installer says so and keeps the file's value; edit the file to change it.
- **The service definition is refreshed.** `/etc/systemd/system/payram-updater.service` is rewritten on every run (the old one is saved to `/etc/payram/installer-backups/` when it differs). Put local changes in a drop-in instead: `sudo systemctl edit payram-updater`.
- **A running PayRam update is never interrupted.** Before it restarts anything, the installer checks the updater's status. If an update is in progress it waits for it to finish (checking every 10 seconds, up to 30 minutes). If it is still running after that, the installer stops without changing anything.
- **The new binary has to prove itself.** It is checksum-verified and run once on your server before it replaces the current one, which is kept as `/usr/local/bin/payram-updater.previous`. After the restart the updater must answer on `http://127.0.0.1:2567/health` within 60 seconds. If it does not, the previous binary is put back and restarted, and the installer exits with an error saying so.

### Installer options

Pass options to `bash` (through `sudo`), not to `curl`. A variable placed before `curl` only reaches `curl`:

```bash
curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo PAYRAM_UPDATER_VERSION=v1.2.3 bash
```

| Option | Default | What it does |
|--------|---------|--------------|
| `PAYRAM_UPDATER_VERSION` | latest release | Install this release tag |
| `PAYRAM_UPDATER_SHA256` | the release's `.sha256` file | Expected SHA-256 of the binary, given out of band |
| `ENABLE_SERVICE` | `true` | `false` installs without enabling or restarting the service |
| `FORCE_REINSTALL` | `false` | Skip the prompts (the env file is still kept and merged, never regenerated) |
| `QUIET` | `false` | No progress output (errors are still printed) |
| `UPDATER_PORT`, `POLICY_URL`, `RUNTIME_MANIFEST_URL`, `EXECUTION_MODE`, ... | see [Configuration](#configuration) | Written to a new env file, or added when the file does not have them |

### Pinning a version and its checksum

The installer always checks the downloaded binary against a SHA-256 checksum and refuses to install it if the checksum is missing or does not match. Every release publishes a `SHA256SUMS` file with the checksums of all binaries, plus a `<binary>.sha256` file next to each binary.

To pin everything, take the installer script from the same tag and give the checksum you got separately (for example from that release's `SHA256SUMS`):

```bash
curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/<tag>/setup_payram_updater.sh \
  | sudo PAYRAM_UPDATER_VERSION=<tag> PAYRAM_UPDATER_SHA256=<sha256 of payram-updater-linux-amd64> bash
```

Use the binary name for your server: `payram-updater-linux-amd64`, `payram-updater-linux-arm64`, `payram-updater-darwin-amd64` or `payram-updater-darwin-arm64`. Release binaries are statically linked, so they do not depend on your distribution's glibc. Tags published before this installer contain the older script, which does not have the safety checks above.

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

### Check which version is installed
```bash
payram-updater version
```

### Check service health
```bash
curl http://127.0.0.1:2567/health
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

### Diagnose system health
```bash
payram-updater inspect
```

This shows:
- Current system state (OK, DEGRADED, or BROKEN)
- Detected issues and their severity
- Recovery recommendations

### Attempt automatic recovery
```bash
payram-updater recover
```

This will attempt to recover from a failed upgrade automatically. Some failures (like database migration errors) require manual intervention for safety.

### View recovery guidance
```bash
curl http://127.0.0.1:2567/upgrade/playbook
```

Shows detailed recovery steps for the current failure.

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

The service is configured via environment variables in `/etc/payram/updater.env`. The installer writes it on a new install and only adds missing settings afterwards. See `packaging/examples/updater.env.example` for a complete, commented template.

### Core Settings

| Setting | Default (as written by the installer) | Description |
|---------|---------|-------------|
| `UPDATER_PORT` | `2567` | HTTP API port |
| `POLICY_URL` | `https://raw.githubusercontent.com/PayRam/payram-scripts/refs/heads/main/updater-configs/upgrade-policy.json` | Upgrade policy JSON URL (required) |
| `RUNTIME_MANIFEST_URL` | `https://raw.githubusercontent.com/PayRam/payram-scripts/refs/heads/main/updater-configs/runtime-manifest.json` | Container manifest JSON URL (required) |
| `EXECUTION_MODE` | `execute` | `execute` performs upgrades; `dry-run` only validates them |
| `STATE_DIR` | `/var/lib/payram-updater` | Job state, logs and history |
| `FETCH_TIMEOUT_SECONDS` | `10` | HTTP request timeout |
| `HEALTH_CHECK_TIMEOUT_SECONDS` | `600` | How long an upgrade waits for the new PayRam version to report healthy before switching back |
| `DOCKER_BIN` | `docker` | Docker binary path |
| `SUPERVISOR_EXCLUDE` | `postgres,postgresql` | Programs in the PayRam container never stopped for the pre-upgrade backup |
| `SUPERVISOR_INCLUDE` | (empty) | If set, only these programs are stopped |

### Database Backup Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `BACKUP_DIR` | `/var/lib/payram/backups` | Backup storage directory |
| `BACKUP_RETENTION` | `10` | Number of backups to keep |
| `BACKUP_TIMEOUT_SECONDS` | `600` | Time limit for the pre-upgrade backup |

The database credentials are read from the PayRam container, so no `PG_*` settings are needed.

### Advanced Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `DEBUG_VERSION_MODE` | `false` | Allow arbitrary version strings (testing) |
| `IMAGE_REPO_OVERRIDE` | (none) | Override image repository for testing |
| `TARGET_CONTAINER_NAME` | (auto-detect) | Override target container name |

To reconfigure, edit the file and restart the service. Check `payram-updater status` first: restart only when no update is in progress.
```bash
sudo nano /etc/payram/updater.env
payram-updater status
sudo systemctl restart payram-updater
```

To change the service itself (for example to add an environment variable or a resource limit), use a drop-in, which the installer leaves alone:
```bash
sudo systemctl edit payram-updater
```

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
curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo bash -s -- --uninstall
```

This removes only the updater's own files: the binary (and `payram-updater.previous`), the service, `/etc/payram/updater.env` (and `/etc/payram/installer-backups/`) and the state directory `/var/lib/payram-updater`. If an update is in progress it waits for it to finish before stopping the updater. Add `--yes` to skip the confirmation prompt.

**Your database backups in `/var/lib/payram/backups` are kept.** To delete them as well, add `--remove-backups`; that removes only that folder.

To uninstall by hand:

```bash
payram-updater status    # make sure no update is in progress
sudo systemctl stop payram-updater
sudo systemctl disable payram-updater
sudo rm /etc/systemd/system/payram-updater.service
sudo systemctl daemon-reload
sudo rm -f /usr/local/bin/payram-updater /usr/local/bin/payram-updater.previous
sudo rm -rf /etc/payram/updater.env /etc/payram/installer-backups /var/lib/payram-updater
```

Do not remove `/var/lib/payram`: it holds the database backups (`/var/lib/payram/backups`). Delete that folder only if you are sure you no longer need them.

## Support

For issues or questions:
- Check logs: `sudo journalctl -u payram-updater -f`
- Run diagnostics: `payram-updater inspect`
- Contact: [sales@payram.com](mailto:sales@payram.com)
