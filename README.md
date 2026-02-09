# PayRam Updater

Manages PayRam runtime upgrades with automated rollback and recovery capabilities.

## Quick Start

Download and run the installation script:

```bash
curl -fsSL https://raw.githubusercontent.com/PayRam/payram-updates/main/setup_payram_updater.sh | sudo bash
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

The service is configured via environment variables in `/etc/payram/updater.env`.

Common settings:

| Setting | Default | Description |
|---------|---------|-------------|
| `UPDATER_PORT` | `2567` | HTTP API port |
| `DEBUG_VERSION_MODE` | `false` | Enable debug logging |

To reconfigure:
```bash
sudo nano /etc/payram/updater.env
sudo systemctl restart payram-updater
```

## View Service Logs

```bash
sudo journalctl -u payram-updater -f
```

## HTTP API

The service provides an HTTP API on port `2567` (default). The API is primarily used by the PayRam dashboard.

### Endpoints

**Health check**
```bash
curl http://127.0.0.1:2567/health
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

**Initiate an upgrade**
```bash
curl -X POST http://127.0.0.1:2567/upgrade \
  -H "Content-Type: application/json" \
  -d '{"requestedTarget":"1.7.8"}'
```

**System diagnostics**
```bash
curl http://127.0.0.1:2567/upgrade/inspect
```

## Uninstall

```bash
sudo systemctl stop payram-updater
sudo systemctl disable payram-updater
sudo rm /etc/systemd/system/payram-updater.service
sudo rm /usr/local/bin/payram-updater
sudo rm -rf /etc/payram /var/lib/payram /var/log/payram
sudo systemctl daemon-reload
```

## Support

For issues or questions:
- Check logs: `sudo journalctl -u payram-updater -f`
- Run diagnostics: `payram-updater inspect`
- Contact: [sales@payram.com](mailto:sales@payram.com)
