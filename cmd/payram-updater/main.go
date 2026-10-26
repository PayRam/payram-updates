package main

import (
	"fmt"
	"os"
)

// version is the release this binary was built from. Release builds set it
// with -ldflags "-X main.version=<tag>"; local builds report "dev".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		// Default command is "serve"
		runServe()
		return
	}

	command := os.Args[1]
	// Handle help flags
	if command == "-h" || command == "--help" || command == "help" {
		printHelp()
		return
	}

	switch command {
	case "version", "--version":
		fmt.Println(version)
	case "init":
		runInit()
	case "serve":
		runServe()
	case "restart":
		runRestart()
	case "status":
		runStatus()
	case "logs":
		runLogs()
	case "dry-run":
		runDryRun()
	case "run":
		runRun()
	case "inspect":
		runInspect()
	case "recover":
		runRecover()
	case "backup":
		runBackup()
	case "cleanup":
		runCleanup()
	case "sync":
		runSync()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printHelp()
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Print(`payram-updater - Payram runtime upgrade manager

USAGE:
  payram-updater [COMMAND]

COMMANDS:
	init             Initialize updater configuration
  serve            Start the upgrade daemon (default)
  restart          Restart the payram-updater systemd service
  status           Get current upgrade status
  logs             Get upgrade logs
  dry-run          Validate upgrade (read-only, no changes)
  run              Execute an upgrade via the daemon
  inspect          Read-only system diagnostics
  recover          Attempt automated recovery from a failed upgrade
  sync             Sync internal state after external upgrade
  backup           Manage database backups (create, list, restore)
	cleanup          Cleanup local state or backups (requires confirmation)
  version          Print the updater version
  help             Show this help message

DRY-RUN FLAGS:
  --mode string    Upgrade mode: 'dashboard' or 'manual' (default: manual)
  --to string      Target version (required)

RESTART:
  Restarts the payram-updater systemd service. Useful when:
  - The service started before Docker and couldn't discover the container
  - Configuration changes require a service reload
  - The service needs to re-scan for Payram containers
  
  Requires: sudo access and systemd

RUN FLAGS:
  --mode string    Upgrade mode: 'dashboard' or 'manual' (default: manual)
  --to string      Target version (required)
  --yes            Skip confirmation prompts and return once the update has
                   started (default: false). Without it, on a terminal, run
                   follows the update and asks before restoring a backup.

LOGS FLAGS:
	-f, --follow     Follow logs (like tail -f)

BACKUP SUBCOMMANDS:
  backup create           Create a new database backup manually
  backup list             List all available backups
  backup restore --file   Restore from a backup (asks before changing anything; --mode db-only|full --yes skips the questions)

BACKUP FLAGS:
  --file string    Path to backup file (for restore)
  --yes            Skip confirmation prompt (for restore)

CLEANUP SUBCOMMANDS:
	cleanup state      Clear updater state (status/logs/history)
	cleanup backups    Clear all backup files

CLEANUP FLAGS:
	--yes            Skip confirmation prompt (type "yes" otherwise)
	Note: Cleanup is blocked if a job is active.

EXAMPLES:
	payram-updater init
  payram-updater serve
  payram-updater restart
  payram-updater status
	payram-updater logs
	payram-updater logs -f
	payram-updater dry-run --to latest
	payram-updater dry-run --mode dashboard --to 1.7.0
	payram-updater run --to latest
	payram-updater run --to 1.2.3 --yes
	payram-updater run --mode dashboard --to latest
  payram-updater inspect
  payram-updater recover
  payram-updater sync
  payram-updater backup create
  payram-updater backup list
  payram-updater backup restore --file /path/to/backup.dump

  payram-updater cleanup state
  payram-updater cleanup backups --yes
  payram-updater version

CONFIG:
  Configuration is loaded from environment variables first,
  then from /etc/payram/updater.env if it exists.

`)
}
