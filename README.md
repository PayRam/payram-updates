# payram-updater

A Go service that manages Payram runtime upgrades via HTTP API.

## Features

- RESTful HTTP API for upgrade management
- GitHub-based policy and manifest fetching
- Dry-run upgrade orchestration
- Dashboard upgrade restrictions with breakpoint enforcement
- Persistent job state and log storage
- Systemd integration for production deployment

## Installation

### Prerequisites

- Linux system with systemd
- Go 1.21+ (for building from source)
- Docker (for running Payram containers)

### Binary Installation

1. Build and install the binary:
```bash
make build
sudo cp bin/payram-updater /usr/local/bin/
sudo chmod +x /usr/local/bin/payram-updater
```

2. Create configuration directory and file:
```bash
sudo mkdir -p /etc/payram
sudo cp packaging/examples/updater.env.example /etc/payram/updater.env
```

3. Edit `/etc/payram/updater.env` with your configuration:
```bash
sudo nano /etc/payram/updater.env
```

Required variables:
- `POLICY_URL`: URL to the policy.json file (e.g., GitHub raw URL)
- `RUNTIME_MANIFEST_URL`: URL to the manifest.json file (e.g., GitHub raw URL)

4. Create state and log directories:
```bash
sudo mkdir -p /var/lib/payram /var/log/payram
sudo chmod 755 /var/lib/payram /var/log/payram
```

5. Install and start the systemd service:
```bash
sudo cp packaging/systemd/payram-updater.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable payram-updater
sudo systemctl start payram-updater
```

6. Verify the service is running:
```bash
sudo systemctl status payram-updater
curl http://127.0.0.1:2359/health
```

Expected response:
```json
{"status":"healthy"}
```

## API Usage

The service listens on `127.0.0.1:2359` by default (configurable via `UPDATER_PORT`).

### Check Service Health

```bash
curl http://127.0.0.1:2359/health
```

Response:
```json
{"status":"healthy"}
```

### Get Upgrade Status

```bash
curl http://127.0.0.1:2359/upgrade/status
```

Response when no job exists:
```json
{
  "state": "IDLE",
  "mode": "",
  "requested_target": "",
  "resolved_target": "",
  "error_code": "",
  "error_message": ""
}
```

### Get Upgrade Logs

```bash
curl http://127.0.0.1:2359/upgrade/logs
```

Response:
```json
{
  "logs": []
}
```

### Initiate Upgrade (Dry-run)

Dashboard mode (policy-controlled):
```bash
curl -X POST http://127.0.0.1:2359/upgrade \
  -H "Content-Type: application/json" \
  -d '{"mode":"DASHBOARD","requested_target":"latest"}'
```

Manual mode (user-controlled):
```bash
curl -X POST http://127.0.0.1:2359/upgrade \
  -H "Content-Type: application/json" \
  -d '{"mode":"MANUAL","requested_target":"v1.2.3"}'
```

Response:
```json
{
  "message": "Upgrade job created successfully. State: READY"
}
```

### Error Responses

Concurrent upgrade attempt:
```json
{
  "error": "An upgrade job is already in progress"
}
```

Dashboard upgrade blocked by breakpoint:
```json
{
  "state": "FAILED",
  "mode": "DASHBOARD",
  "requested_target": "latest",
  "resolved_target": "v1.2.3",
  "error_code": "MANUAL_UPGRADE_REQUIRED",
  "error_message": "Dashboard upgrades to v1.2.3 are not allowed: Breaking database schema changes. See: https://docs.example.com/migration-guide"
}
```

## Configuration

Environment variables (set in `/etc/payram/updater.env`):

| Variable | Default | Description |
|----------|---------|-------------|
| `UPDATER_PORT` | `2359` | HTTP server port |
| `POLICY_URL` | (required) | URL to policy.json |
| `RUNTIME_MANIFEST_URL` | (required) | URL to manifest.json |
| `FETCH_TIMEOUT_SECONDS` | `10` | HTTP client timeout |
| `STATE_DIR` | `/var/lib/payram` | Job state directory |
| `LOG_DIR` | `/var/log/payram` | Job log directory |

## Development

### Build

```bash
make build
```

### Run Tests

```bash
make test
```

### Run All Pre-commit Checks

```bash
make precommit-checks
```

This runs:
- `go fmt` (formatting)
- `go vet` (static analysis)
- `go test` (all tests)
- `go build` (compilation)

## Architecture

### Job States

- `IDLE`: No active job
- `POLICY_FETCHING`: Fetching policy from GitHub
- `MANIFEST_FETCHING`: Fetching manifest from GitHub
- `READY`: Dry-run complete, ready for actual upgrade
- `FAILED`: Job failed (check error_code/error_message)

### Upgrade Modes

- `DASHBOARD`: Policy-controlled upgrades with breakpoint enforcement
  - Fails immediately if policy fetch fails
  - Checks breakpoints and blocks matching versions
  - Recommended for production dashboard-initiated upgrades

- `MANUAL`: User-controlled upgrades with warnings
  - Logs policy fetch failures but continues
  - Ignores breakpoints
  - Allows any target version
  - Recommended for manual operator interventions

### Breakpoint Enforcement

Breakpoints in the policy file prevent dashboard upgrades to specific versions:

```json
{
  "breakpoints": [
    {
      "version": "v1.2.3",
      "reason": "Breaking database schema changes",
      "docs": "https://docs.example.com/migration-guide"
    }
  ]
}
```

Dashboard mode will fail with `MANUAL_UPGRADE_REQUIRED` if the resolved target matches a breakpoint. Manual mode ignores breakpoints.

## Systemd Service

The service runs as `root` with the following characteristics:

- **User/Group**: root (required for Docker operations)
- **Restart Policy**: Always (with 10-second delay)
- **Security Hardening**:
  - `NoNewPrivileges=false` (allows Docker execution)
  - `PrivateTmp=true` (isolated /tmp)
  - `ProtectSystem=strict` (read-only system directories)
  - `ProtectHome=true` (no home directory access)
  - `ReadWritePaths=/var/lib/payram /var/log/payram` (state/log access)

View service logs:
```bash
sudo journalctl -u payram-updater -f
```

## License

Proprietary - Payram Inc.
