# Installer tests

`run.sh` runs `setup_payram_updater.sh` from this checkout on fresh Linux
servers and checks the one promise the installer makes:

> after it runs, the server has a working updater: the service is enabled and
> answers, the merchant's settings are kept, and when the new build cannot
> run, the previous updater is put back.

```bash
test/installer/run.sh                         # every case (about 15 minutes)
test/installer/run.sh upgrade_over_released_v0_2_3
INSTALLER_DISTROS="ubuntu:22.04" test/installer/run.sh fresh_install_as_root_without_sudo
```

Each server is a privileged container that boots systemd and runs its own
Docker daemon, so nothing touches the Docker that runs the script; a busybox
container tagged `payramapp/payram` stands in for PayRam for the installer's
container-to-updater connectivity check. The build from this checkout is
installed from a `file://` URL. The released installer and the released
updater come from GitHub, as merchants get them. Needs Docker (privileged
containers), Go and internet access. Run it before every updater release and
whenever the installer changes.

| Case | Server | Expects |
|---|---|---|
| `fresh_install_as_root_without_sudo` | Ubuntu 24.04, Debian 12, Ubuntu 20.04; root, no `sudo` installed | installs; service enabled and answering; defaults written; state and backup folders mode 700, CLI token 600; the PayRam container reaches the updater (the released installer stops at "Missing dependency: sudo" here) |
| `upgrade_over_released_v0_2_3` | Ubuntu 24.04 with the released installer's v0.2.3 and the merchant's own settings | upgrades and keeps every setting, v0.2.3 kept as `payram-updater.previous`; a second run changes nothing; with an update in progress it gives up without touching anything; a new build that cannot start is replaced by the previous one |
| `called_by_setup_payram` | Ubuntu 24.04 | installed exactly as `setup_payram.sh` calls it (quiet, forced, from a terminal, output discarded), and again; uninstall keeps the backups; installs again |
| `custom_port_kept` | an env file with `UPDATER_PORT=3000` | the port is kept and used; missing settings added once |
| `github_api_unreachable` | `api.github.com` blocked | downloads the latest release directly and installs it |
| `non_root_user_with_sudo` | Debian 12, an admin user with sudo | installs through sudo; manual mode without sudo is refused with a hint to use sudo |

Servers are named `payram-installer-*`, with volumes of the same names; each
case removes its own. Images `payram-installer-test:*` are kept for the next
run (remove them with `docker rmi $(docker images -q payram-installer-test)`).
