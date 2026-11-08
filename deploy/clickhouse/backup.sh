#!/usr/bin/env bash
# Nightly backup of the cliproxy-log database to the NAS.
#
# 1. ClickHouse writes a consistent copy of the database with BACKUP DATABASE to
#    its backups disk (~/.local/share/cliproxy-log/backups/cliproxy_log).
# 2. restic uploads that directory to its own encrypted repository on the NAS over
#    SFTP, the same transport the fleet credential backup uses, but a separate
#    repository and password.
# 3. Old snapshots are pruned (7 daily, 4 weekly, 6 monthly), a sample of the
#    repository is verified, and the local copy is deleted.
#
# install.sh installs this as ~/.local/bin/cliproxy-log-backup, run nightly by
# cliproxy-log-backup.timer. Run it by hand the same way. With --check-docker it
# only checks that it can reach Docker (install.sh uses this) and exits.
#
# Environment overrides (defaults in brackets):
#   CLIPROXY_LOG_CONFIG_DIR         passwords [~/.config/cliproxy-log]
#   CLIPROXY_LOG_DATA_DIR           backup staging [~/.local/share/cliproxy-log]
#   CLIPROXY_LOG_BACKUP_HOST        snapshot host name [uname -n, short]
#   CLIPROXY_LOG_NAS                SSH alias of the NAS [storageserver]
#   CLIPROXY_LOG_RESTIC_REPOSITORY  full restic repository
#                                   [sftp:$NAS:/home/cliproxy-log-backups/$HOST]
set -euo pipefail

config_dir="${CLIPROXY_LOG_CONFIG_DIR:-$HOME/.config/cliproxy-log}"
data_dir="${CLIPROXY_LOG_DATA_DIR:-$HOME/.local/share/cliproxy-log}"
host="${CLIPROXY_LOG_BACKUP_HOST:-$(uname -n)}"
host="${host%%.*}"
nas="${CLIPROXY_LOG_NAS:-storageserver}"
# Synology's SFTP root is a virtual tree; /home there is the login's home folder.
repository="${CLIPROXY_LOG_RESTIC_REPOSITORY:-sftp:$nas:/home/cliproxy-log-backups/$host}"
container=cliproxy-clickhouse
backup_name=cliproxy_log
staging="$data_dir/backups/$backup_name"
tag=cliproxy-log

log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }

# The timer runs under the systemd user manager, which keeps the groups it started
# with. With lingering it is never restarted, so a later `usermod -aG docker` does
# not reach it short of a reboot. When the Docker socket's group lists this user but
# this process lacks it, rerun through sg, which grants a listed member that group
# without a password. Docker reached any other way is used as is.
socket=/var/run/docker.sock
if [ -z "${DOCKER_HOST:-}" ] && [ -z "${CLIPROXY_LOG_SG:-}" ] && [ -S "$socket" ] && ! docker info >/dev/null 2>&1; then
  group="$(stat -c %G "$socket")"
  if ! id -nG | tr ' ' '\n' | grep -qx "$group" && id -nG "$(id -un)" | tr ' ' '\n' | grep -qx "$group"; then
    log "rerunning through sg $group to reach Docker"
    CLIPROXY_LOG_SG=1 exec sg "$group" -c "$(printf '%q ' "$0" "$@")"
  fi
fi
if [ "${1:-}" = --check-docker ]; then
  docker info >/dev/null 2>&1 || { log "cannot reach Docker"; exit 1; }
  log "Docker is reachable"
  exit 0
fi

# Never inherit restic settings from the caller; everything is passed explicitly.
unset RESTIC_REPOSITORY RESTIC_PASSWORD RESTIC_PASSWORD_COMMAND
export RESTIC_PASSWORD_FILE="$config_dir/restic-password"
restic_args=(--repo "$repository")
if [[ "$repository" == sftp:* ]]; then
  # Unattended SSH: never prompt, give up on a dead peer instead of hanging.
  # sftp.args adds to restic's own ssh command, which keeps the repository's host and user.
  restic_args+=(-o "sftp.args=-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4")
fi

command -v restic >/dev/null || { log "restic is not installed"; exit 1; }
[ -s "$RESTIC_PASSWORD_FILE" ] || { log "missing $RESTIC_PASSWORD_FILE; run install.sh"; exit 1; }

# One run at a time; a second run exits instead of waiting.
exec 9>"$data_dir/backup.lock"
flock -n 9 || { log "another backup is running"; exit 1; }

cleanup() { rm -rf "$staging"; }
trap cleanup EXIT
cleanup # a killed earlier run may have left a copy behind

admin_client() {
  CLICKHOUSE_PASSWORD="$(cat "$config_dir/admin-password")" \
    docker exec -i -e CLICKHOUSE_USER=cliproxy_admin -e CLICKHOUSE_PASSWORD "$container" \
    clickhouse-client "$@"
}

log "creating ClickHouse backup"
status="$(admin_client -q "BACKUP DATABASE cliproxy_log TO Disk('backups', '$backup_name') FORMAT TSV" | cut -f2)"
[ "$status" = BACKUP_CREATED ] || { log "BACKUP DATABASE ended with status '$status'"; exit 1; }
log "backup size: $(du -sh "$staging" | cut -f1)"

if ! restic "${restic_args[@]}" cat config >/dev/null 2>&1; then
  log "initializing restic repository $repository"
  restic "${restic_args[@]}" init
fi

log "uploading to $repository"
# The staging path is the same every night, so restic sees one growing history of it.
restic "${restic_args[@]}" backup --host "$host" --tag "$tag" "$staging"

log "applying retention"
restic "${restic_args[@]}" forget --host "$host" --tag "$tag" \
  --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune

log "verifying a sample of the repository"
restic "${restic_args[@]}" check --read-data-subset 5%

log "done"
