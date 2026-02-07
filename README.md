# payram-updater

A Go service that manages Payram runtime upgrades via HTTP API.

## Architecture

**STATELESS DESIGN**: This updater does not persist or assume any installation-time configuration. All runtime details (ports, environment variables, volumes, networks) are discovered via live Docker container inspection and overlaid with manifest settings. Only job state, logs, and database backups are persisted.

**No Script Assumptions**: The updater makes no assumptions about how Payram was installed. It does not load config files from disk, mount installation configs, or assume fixed container names.

**Source of Truth**: Docker inspection + manifest overlays = complete runtime configuration.

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

The service listens on port `2359` by default (configurable via `UPDATER_PORT`) and binds to all interfaces to allow access from both localhost and Docker containers.

**Access URLs:**
- From localhost: `http://127.0.0.1:2359`
- From Docker containers: `http://<docker-bridge-ip>:2359` (typically `http://172.17.0.1:2359`)

The Docker bridge IP is automatically detected and logged on startup.

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

### Get History (Upgrades/Backups/Restores)

```bash
curl http://127.0.0.1:2359/history
curl "http://127.0.0.1:2359/history?type=upgrade&status=failed&limit=50"
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
  -d '{"mode":"MANUAL","requested_target":"1.2.3"}'
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
  "resolved_target": "1.2.3",
  "error_code": "MANUAL_UPGRADE_REQUIRED",
  "error_message": "Dashboard upgrades to 1.2.3 are not allowed: Breaking database schema changes. See: https://docs.example.com/migration-guide"
}
```

## Configuration

Environment variables (set in `/etc/payram/updater.env`):

| Variable | Default | Description |
|----------|---------|-------------|
| `UPDATER_PORT` | `2567` | HTTP server port |
| `POLICY_URL` | (required) | URL to policy.json |
| `RUNTIME_MANIFEST_URL` | (required) | URL to manifest.json |
| `FETCH_TIMEOUT_SECONDS` | `10` | HTTP client timeout |
| `STATE_DIR` | `/var/lib/payram` | Job state directory |
| `LOG_DIR` | `/var/log/payram` | Job log directory |
| `TARGET_CONTAINER_NAME` | `payram` | Docker container name to upgrade |
| `CORE_BASE_URL` | `http://127.0.0.1:8080` | Payram core API base URL |
| `EXECUTION_MODE` | `dry-run` | Execution mode: `dry-run` or `execute` |
| `DOCKER_BIN` | `docker` | Path to docker binary |
| `IMAGE_REPO_OVERRIDE` | (none) | Override manifest image repo (for local testing) |
| `SKIP_DOCKER_PULL` | `false` | Skip docker pull (for local testing with local images) |
| `PORTS_OVERRIDE` | (none) | Override manifest ports (format: `host:container,host:container,...`) |




The updater enforces this constraint and will fail if any code attempts to mount the config file as a volume.

## CLI Commands

The `payram-updater` binary provides CLI commands for interacting with the daemon.

### Upgrade Commands

#### dry-run (Read-only validation)

Validates an upgrade without making any changes:

```bash
payram-updater dry-run --mode dashboard --to 1.7.0
payram-updater dry-run --mode manual --to 1.5.0
```

- Fetches policy and manifest
- Validates target version against breakpoints
- Resolves "latest" to actual version
- **Does NOT create jobs or modify state**
- Exit code 0 on success, 1 on validation failure

#### run (Execute upgrade)

Executes an upgrade via the daemon. **Interactive by default**.

```bash
# Interactive mode (prompts for confirmation)
payram-updater run --mode dashboard --to 1.7.0

# Non-interactive mode (for automation)
payram-updater run --mode manual --to 1.5.0 --yes
```

Behavior:
1. **Validation**: Calls `/upgrade/plan` to validate the request
2. **Confirmation**: Shows a summary and prompts `Proceed? (y/N):` unless `--yes` is set
3. **Execution**: Calls `/upgrade/run` to start the upgrade job
4. **Returns immediately** after job is started (async execution)

Exit codes:
- `0`: Success (job started) or user aborted cleanly
- `1`: Validation failed, execution failed, or conflict (job already running)
- `2`: Non-interactive mode without `--yes` flag

Flags:
- `--mode` (required): `dashboard` or `manual`
- `--to` (required): Target version (e.g., `1.7.0`, or `latest` for manual mode only)
- `--yes`: Skip confirmation prompt (required for non-interactive/automated use)

Example output:
```
╔══════════════════════════════════════════════════════════════╗
║                     UPGRADE SUMMARY                          ║
╠══════════════════════════════════════════════════════════════╣
║  Mode:             DASHBOARD                                 ║
║  Requested Target: 1.7.0                                     ║
║  Image:            ghcr.io/payram/runtime                    ║
║  Container:        payram-core                               ║
╠══════════════════════════════════════════════════════════════╣
║  ⚠️  This will stop and replace the container.               ║
║     Brief downtime expected.                                 ║
║                                                              ║
║  ℹ️  Dashboard upgrades may be blocked by policy breakpoints.║
╚══════════════════════════════════════════════════════════════╝

Proceed? (y/N): y
Started upgrade job job-1234567890 (state=EXECUTING).
Use 'payram-updater status' to check progress and 'payram-updater logs -f' to follow progress.
```

### Other Commands

```bash
payram-updater status    # Get current upgrade status
payram-updater logs      # Get upgrade logs (snapshot)
payram-updater logs -f   # Follow upgrade logs
payram-updater inspect   # Read-only system diagnostics
payram-updater recover   # Attempt automated recovery
payram-updater backup    # Manage database backups
```

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
- `READY`: Dry-run complete, ready for actual upgrade (or upgrade completed successfully)
- `EXECUTING`: Performing Docker operations (pull, stop, remove, run)
- `VERIFYING`: Verifying deployment (health, version, migrations)
- `FAILED`: Job failed (check failure_code/message)

### Execution Modes

- `dry-run` (default): Validates policy and manifest, logs planned actions without making changes
- `execute`: Performs actual Docker operations and verifies deployment

### Failure Codes

- `POLICY_FETCH_FAILED`: Failed to fetch policy (DASHBOARD mode only)
- `POLICY_INVALID_JSON`: Policy JSON parsing failed
- `MANIFEST_FETCH_FAILED`: Failed to fetch manifest
- `MANIFEST_INVALID_JSON`: Manifest JSON parsing failed
- `MANUAL_UPGRADE_REQUIRED`: Breakpoint hit (DASHBOARD mode only)
- `DOCKER_ERROR`: Docker operation failed (pull/stop/remove/run/inspect)
- `HEALTHCHECK_FAILED`: Health endpoint verification failed after retries
- `VERSION_MISMATCH`: Deployed version doesn't match target
- `MIGRATIONS_FAILED`: Database migrations incomplete

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
      "version": "1.2.3",
      "reason": "Breaking database schema changes",
      "docs": "https://docs.example.com/migration-guide"
    }
  ]
}
```

Dashboard mode will fail with `MANUAL_UPGRADE_REQUIRED` if the resolved target matches a breakpoint. Manual mode ignores breakpoints.

## Recovery & Troubleshooting

When an upgrade fails, the payram-updater provides comprehensive recovery guidance through playbooks and diagnostic tools.

### Recovery Commands

**Inspect system state:**
```bash
payram-updater inspect
```

This performs read-only diagnostics and returns:
- Overall state: `OK`, `DEGRADED`, or `BROKEN`
- Issues detected with severity levels
- Recovery recommendations
- Attached recovery playbook (if applicable)

**Attempt automated recovery:**
```bash
payram-updater recover
```

This attempts to recover from a failed upgrade. Note that some failures (like `MIGRATION_FAILED`) refuse automated recovery to protect data integrity.

### Recovery Playbooks

Each failure code has an associated playbook with:
- **Severity**: `INFO`, `RETRYABLE`, or `MANUAL_REQUIRED`
- **Data Risk**: `NONE`, `POSSIBLE`, `LIKELY`, or `UNKNOWN`
- **SSH Steps**: Manual recovery commands
- **Documentation URL**: Detailed guidance

#### Failure Codes and Severity

| Code | Severity | Data Risk | Description |
|------|----------|-----------|-------------|
| `POLICY_FETCH_FAILED` | RETRYABLE | NONE | Network issue fetching policy |
| `MANIFEST_FETCH_FAILED` | RETRYABLE | NONE | Network issue fetching manifest |
| `DOCKER_PULL_FAILED` | RETRYABLE | NONE | Docker image pull failed |
| `DOCKER_ERROR` | MANUAL_REQUIRED | POSSIBLE | Docker daemon or container error |
| `HEALTHCHECK_FAILED` | MANUAL_REQUIRED | POSSIBLE | Application failed health checks |
| `VERSION_MISMATCH` | MANUAL_REQUIRED | POSSIBLE | Deployed version doesn't match target |
| `MIGRATION_FAILED` | MANUAL_REQUIRED | LIKELY | Database migration error |
| `DISK_SPACE_LOW` | MANUAL_REQUIRED | UNKNOWN | Insufficient disk space |
| `CONCURRENCY_BLOCKED` | RETRYABLE | NONE | Another upgrade in progress |
| `MANUAL_UPGRADE_REQUIRED` | MANUAL_REQUIRED | UNKNOWN | Breakpoint hit |

#### Critical: MIGRATION_FAILED

`MIGRATION_FAILED` is special - automated recovery is **always refused** for this failure code because:
- Database migrations may have partially applied
- Data integrity could be at risk
- Manual inspection is required before any recovery action

When encountering `MIGRATION_FAILED`:
1. Check `/upgrade/logs` for migration details
2. Inspect the database state manually
3. Follow the playbook's SSH steps
4. Contact Payram support if uncertain

### API Endpoints for Recovery

**Get last job state:**
```bash
curl http://127.0.0.1:2359/upgrade/last
```

**Get current recovery playbook:**
```bash
curl http://127.0.0.1:2359/upgrade/playbook
```

**Run full system inspection:**
```bash
curl http://127.0.0.1:2359/upgrade/inspect
```

## Backups & Restore

The payram-updater automatically creates database backups before each upgrade to enable safe recovery.

### Configuration

Backup configuration via environment variables:

**NOTE: Backups are always enabled.** This is a safety requirement to enable recovery from failed upgrades.

```bash
# Directory for backup files
BACKUP_DIR=data/backups  # Local dev
# BACKUP_DIR=/var/lib/payram/backups  # Production

# Number of backups to retain
BACKUP_RETENTION=10

# PostgreSQL connection
PG_HOST=127.0.0.1
PG_PORT=5432
PG_DB=payram
PG_USER=payram
PG_PASSWORD=your_password
```

### Backup Commands

**Create a manual backup:**
```bash
payram-updater backup create
```

Output:
```json
{
  "success": true,
  "backup": {
    "id": "20260127-143052-1.7.8",
    "path": "data/backups/payram-backup-20260127-143052-1.7.8-to-manual.dump",
    "filename": "payram-backup-20260127-143052-1.7.8-to-manual.dump",
    "size": 12345678,
    "created_at": "2026-01-27T14:30:52Z",
    "database": "payram"
  }
}
```

**List available backups:**
```bash
payram-updater backup list
```

Output:
```json
{
  "success": true,
  "count": 3,
  "backups": [
    {
      "id": "20260127-143052-1.7.8",
      "filename": "payram-backup-20260127-143052-1.7.8-to-1.7.9.dump",
      "size": 12345678,
      "created_at": "2026-01-27T14:30:52Z",
      "from_version": "1.7.8",
      "target_version": "1.7.9"
    }
  ]
}
```

**Restore from a backup:**
```bash
# Interactive (prompts for confirmation)
payram-updater backup restore --file /path/to/backup.dump

# Non-interactive (use in scripts)
payram-updater backup restore --file /path/to/backup.dump --yes
```

⚠️ **WARNING**: Restore replaces ALL current database data with the backup contents.

### Pre-Upgrade Backup Workflow

When an upgrade runs in `execute` mode:

1. State changes to `BACKING_UP`
2. Current version is fetched from running container
3. `pg_dump -Fc` creates a backup file
4. Backup metadata is stored in `backups.json`
5. Old backups are pruned (keeping last `BACKUP_RETENTION`)
6. State changes to `EXECUTING` for the actual upgrade

If backup fails:
- State: `FAILED`, Code: `BACKUP_FAILED`
- No containers are modified (safe failure)
- Retry after fixing the issue (disk space, DB connectivity)

### Recovery with Backups

For failures like `MIGRATION_FAILED`, `HEALTHCHECK_FAILED`, or `VERSION_MISMATCH`:

1. **Find the backup created by the job:**
```bash
payram-updater backup list
# Look for backup matching the failed job's timestamp
```

2. **Stop the broken container:**
```bash
docker stop payram-dummy
docker rm payram-dummy
```

3. **Restore the database:**
```bash
payram-updater backup restore --file /path/to/pre-upgrade-backup.dump --yes
```

4. **Run the previous known-good version:**
```bash
# Get the previous image tag from the backup's from_version
docker run -d --name payram-dummy payram/payram-dummy:1.7.8 ...
```

5. **Verify recovery:**
```bash
curl http://127.0.0.1:18080/health
payram-updater inspect
```

### Example Recovery Flow

1. **Detect failure:**
```bash
payram-updater status
# Shows: state=FAILED, failure_code=HEALTHCHECK_FAILED
```

2. **Get full diagnostics:**
```bash
payram-updater inspect
# Shows: overall_state=BROKEN, issues, recommendations
```

3. **Review playbook:**
```bash
curl http://127.0.0.1:2359/upgrade/playbook | jq .
# Shows recovery steps and severity
```

4. **Attempt recovery (if safe):**
```bash
payram-updater recover
# Either succeeds or explains why it's refused
```

5. **For MIGRATION_FAILED - follow manual steps:**
```bash
# Check logs
payram-updater logs -f

# SSH to server and follow playbook:
# 1. Check PostgreSQL migration state
# 2. Review migration logs
# 3. Determine if rollback is safe
# 4. Contact support if needed
```

## Manual Smoke Testing

For local development and testing, the repository includes test data files in the `data/` directory.

### Test Data Files

- `data/upgrade-policy.json`: Policy with 10 releases and 2 breakpoints:
  - `1.8.0`: Breaking change requiring manual step
  - `1.10.0`: Breaking DB migration requirement
- `data/runtime-manifest.json`: Manifest with version-specific overrides

### Running Smoke Tests Locally

1. **Configure for local file mode** (already set in `.env`):
```bash
POLICY_URL=data/upgrade-policy.json
RUNTIME_MANIFEST_URL=data/runtime-manifest.json
STATE_DIR=data/state
LOG_DIR=data/logs
```

2. **Start the daemon**:
```bash
go run cmd/payram-updater/main.go serve
```

3. **Test health check**:
```bash
curl http://127.0.0.1:2359/health
# Expected: {"status":"ok"}
```

4. **Test dashboard upgrade blocked by breakpoint**:
```bash
payram-updater dry-run --mode dashboard --to 1.8.0
# Expected: State FAILED with MANUAL_UPGRADE_REQUIRED
```

5. **Test manual upgrade bypassing breakpoint**:
```bash
payram-updater dry-run --mode manual --to 1.8.0
# Expected: State READY (breakpoint ignored)
```

6. **Test concurrency guard**:
```bash
# While job active from previous step:
payram-updater dry-run --mode dashboard --to 1.9.0
# Expected: HTTP 409 Conflict error
```

7. **View logs**:
```bash
payram-updater logs -f
```

8. **Test actual upgrade execution** (requires Docker and dummy container):
```bash
# Setup: Start a dummy nginx container for testing
docker run -d --name payram-test nginx:latest

# Configure for execute mode
export EXECUTION_MODE=execute
export TARGET_CONTAINER_NAME=payram-test
export CORE_BASE_URL=http://127.0.0.1:8080
export DOCKER_BIN=docker

# Start updater with execute configuration
go run cmd/payram-updater/main.go serve

# Trigger upgrade (will actually replace container)
curl -X POST http://127.0.0.1:2359/upgrade \
  -H "Content-Type: application/json" \
  -d '{"mode":"MANUAL","requested_target":"1.9.0"}'

# Monitor execution
curl http://127.0.0.1:2359/upgrade/logs

# Cleanup
docker rm -f payram-test
```

### Local Testing with data-dummy Container

The `data-dummy/` directory contains a Dockerfile for building a realistic test container that mimics the Payram application with proper health and version endpoints.

1. **Build the local dummy image**:
```bash
cd data-dummy
docker build -t payram-dummy:local .
cd ..
```

2. **Create a test environment file**:
```bash

IMAGE_TAG=local
NETWORK_TYPE=bridge
SERVER=test
AES_KEY=test-key-12345678901234567890123456789012
DB_HOST=localhost
DB_PORT=5432
DB_NAME=payram
DB_USER=payram
DB_PASSWORD=testpassword
POSTGRES_SSLMODE=disable
EOF

# Verify config file
```

3. **Configure for local testing with overrides**:
```bash
# Required settings
export EXECUTION_MODE=execute
export TARGET_CONTAINER_NAME=payram-dummy-test

# Local test harness overrides
export IMAGE_REPO_OVERRIDE=payram-dummy
export SKIP_DOCKER_PULL=true

# Port overrides for local testing (maps to different host ports)
export PORTS_OVERRIDE="18080:8080,15432:5432"

# Core URL for local dummy (uses port 18080)
export CORE_BASE_URL=http://127.0.0.1:18080
```

4. **Start the updater**:
```bash
go run cmd/payram-updater/main.go serve
```

5. **Trigger an upgrade**:
```bash
curl -X POST http://127.0.0.1:2359/upgrade \
  -H "Content-Type: application/json" \
  -d '{"mode":"MANUAL","requested_target":"local"}'
```

6. **Verify the container is running**:
```bash
docker ps | grep payram-dummy-test
curl http://127.0.0.1:18080/version
curl http://127.0.0.1:18080/health
```

7. **Cleanup**:
```bash
docker rm -f payram-dummy-test
```

**Local Test Override Environment Variables:**

| Variable | Purpose |
|----------|---------|
| `IMAGE_REPO_OVERRIDE` | Use local image repo instead of manifest's `image.repo` |
| `SKIP_DOCKER_PULL` | Skip `docker pull` for locally-built images |
| `PORTS_OVERRIDE` | Override manifest ports (format: `host:container,host:container,...`) |

### Expected Test Behaviors

- **Dashboard mode**: Fails on breakpoint versions (1.8.0, 1.10.0)
- **Manual mode**: Bypasses breakpoints, allows any version
- **Concurrency**: Only one job allowed at a time (409 if attempted)
- **Policy fetch failure**: Fatal in DASHBOARD mode, warning in MANUAL mode
- **Dry-run mode**: Logs what would be executed without making changes
- **Execute mode**: Performs actual Docker operations and verifies deployment

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
