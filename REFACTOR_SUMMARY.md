# Database Execution Refactor Summary

## Overview
Successfully refactored backup and restore operations to use a shared database execution engine (`internal/dbexec` package). This eliminates code duplication and provides a single source of truth for credential discovery and PostgreSQL operations.

## Changes Made

### 1. New Package: `internal/dbexec`
Created a new shared package that provides:
- **Types**: `DBMode`, `CredentialSource`, `DBCreds`, `DBContext`, `PGExecutor` interface
- **Discovery**: `DiscoverDBContext()` - unified credential resolution with 3-step precedence
- **Executors**: 
  - `DockerPGExecutor` - runs `pg_dump`/`pg_restore` inside containers via `docker exec`
  - `HostPGExecutor` - runs PostgreSQL tools from host with `PGPASSWORD` authentication

### 2. Credential Discovery Flow
The `DiscoverDBContext()` function now handles all credential discovery with this precedence:
1. **Environment variables** (`POSTGRES_HOST` for remote DB) → `CredFromEnv`
2. **Running container** (docker inspect) → `CredFromRunningContainer`
3. **Persisted credentials** (data/state/db.env) → `CredFromPersistedFile`

### 3. Database Execution Modes
- **DBModeInContainer**: Database runs inside Docker container (localhost/127.0.0.1)
  - Uses `docker exec` to run `pg_dump`/`pg_restore` inside container
- **DBModeExternal**: Database on external host (RDS, remote server)
  - Uses host's PostgreSQL tools with connection parameters and `PGPASSWORD`

### 4. Refactored Backup Package
**Before**: `backup.CreateBackup()` had ~80 lines of duplicated credential discovery logic

**After**: Simplified to use `dbexec`:
```go
dbCtx, err := dbexec.DiscoverDBContext(ctx, executor, opts)
var pgExec dbexec.PGExecutor
if dbCtx.Mode == dbexec.DBModeInContainer {
    pgExec = dbexec.NewDockerPGExecutor(executor, logger)
} else {
    pgExec = dbexec.NewHostPGExecutor(executor, logger)
}
err = pgExec.Dump(ctx, dbCtx, backupPath, "custom")
```

**Removed functions**:
- `backupFromContainer()` - replaced by `DockerPGExecutor.Dump()`
- `backupFromHost()` - replaced by `HostPGExecutor.Dump()`
- `getEnvOrDefault()` - now in dbexec package

### 5. Refactored Restore Logic
**Before**: `backup.RestoreBackup()` had ~90 lines of duplicated credential discovery logic

**After**: Simplified to use `dbexec`:
```go
dbCtx, err := dbexec.DiscoverDBContext(ctx, executor, opts)
var pgExec dbexec.PGExecutor
if dbCtx.Mode == dbexec.DBModeInContainer {
    pgExec = dbexec.NewDockerPGExecutor(executor, logger)
} else {
    pgExec = dbexec.NewHostPGExecutor(executor, logger)
}
err = pgExec.Restore(ctx, dbCtx, backupPath, format)
```

**Removed functions**:
- `restoreWithPsql()` - replaced by `PGExecutor.Restore()` with "sql" format
- `restoreWithPgRestore()` - replaced by `PGExecutor.Restore()` with "dump" format

### 6. Import Cycle Resolution
**Problem**: `dbexec` initially imported `backup` for `LoadPersistedCredentials()`, creating a cycle.

**Solution**: Inlined credential loading logic in `dbexec/discovery.go`:
- `loadPersistedCredentials()` - local implementation
- `getContainerDBConfig()` - extracts creds via docker inspect
- `containerDBConfig` - local type to avoid importing backup

## Benefits

1. **Code Reduction**: Eliminated ~170 lines of duplicated code
2. **Single Source of Truth**: One place for credential discovery logic
3. **Better Testing**: Shared logic is tested once in `dbexec` package
4. **Maintainability**: Future changes to credential discovery only need one update
5. **Consistency**: Backup and restore now guaranteed to use identical logic
6. **Clear Separation**: Database operations abstracted from backup/restore business logic

## Test Results
All tests passing:
- ✅ `internal/dbexec`: 4/4 tests pass
- ✅ `internal/backup`: 21/21 tests pass

## Original Problem Solved
✅ **Fixed**: Backup with container running and random port mapping (`55432->5432`)
- Both backup and restore now correctly detect in-container DB via `POSTGRES_HOST=localhost`
- Both use `docker exec` to run PostgreSQL tools inside container
- No more host connection attempts to `localhost:5432` when container uses different ports

## Files Changed
- **New**: `internal/dbexec/types.go` (90 lines)
- **New**: `internal/dbexec/discovery.go` (275 lines)
- **New**: `internal/dbexec/docker_executor.go` (171 lines)
- **New**: `internal/dbexec/host_executor.go` (171 lines)
- **New**: `internal/dbexec/dbexec_test.go` (302 lines)
- **Modified**: `internal/backup/backup.go` (reduced from 936 to ~690 lines)
- **Modified**: `internal/backup/backup_test.go` (updated 1 test assertion)

## Future Improvements
- Add integration tests with real containers for container discovery paths
- Consider exposing error codes as constants for better error handling
- Add metrics/observability for credential discovery steps
- Support for connection pooling configuration
- Support for SSL certificates for remote databases
