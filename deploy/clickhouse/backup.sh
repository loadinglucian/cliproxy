#!/usr/bin/env bash
# Nightly backup of the cliproxy-log database to the NAS.
#
# 1. ClickHouse writes a consistent copy of the database with BACKUP DATABASE to
#    its backups disk (~/.local/share/cliproxy-log/backups/cliproxy_log). The query
#    goes over HTTP on the loopback port as cliproxy_backup, which may only run
#    BACKUP, so this needs no Docker access.
# 2. restic uploads that directory to its own encrypted repository on the NAS over
#    SFTP, the same transport the fleet credential backup uses, but a separate
#    repository and password.
# 3. Old snapshots are pruned (7 daily, 4 weekly, 6 monthly), a sample of the
#    repository is verified, and the local copy is deleted.
#
# install.sh installs this as ~/.local/bin/cliproxy-log-backup, run nightly by
# cliproxy-log-backup.timer. Run it by hand the same way. With --check it only
# checks that the backup user can log in over HTTP and holds BACKUP and WRITE ON
# DISK (install.sh uses this), and exits; --check-docker is an older name for it.
#
# Environment overrides (defaults in brackets):
#   CLIPROXY_LOG_CONFIG_DIR         passwords [~/.config/cliproxy-log]
#   CLIPROXY_LOG_DATA_DIR           backup staging [~/.local/share/cliproxy-log]
#   CLIPROXY_LOG_HTTP_PORT          ClickHouse HTTP port on 127.0.0.1 [8123]
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
url="http://127.0.0.1:${CLIPROXY_LOG_HTTP_PORT:-8123}/"
backup_name=cliproxy_log
staging="$data_dir/backups/$backup_name"
tag=cliproxy-log

log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }

# Runs one query over HTTP as cliproxy_backup and prints the response body, then
# the HTTP status on its own last line (000 when the server cannot be reached). The
# credentials go in headers read from a file descriptor, never argv or the
# environment, and -q and --noproxy keep ~/.curlrc and proxies out.
# wait_end_of_query makes the server hold the response until the query ends, so a
# failure always arrives as an error status. There is no overall deadline: a backup
# takes as long as the database needs.
password_file="$config_dir/backup-password"
backup_query() {
  curl -q --noproxy '*' -sS --connect-timeout 10 -o - -w '\n%{http_code}' \
    -H @<(printf 'X-ClickHouse-User: cliproxy_backup\nX-ClickHouse-Key: %s\n' "$(cat "$password_file")") \
    --data-binary "$1" "$url?wait_end_of_query=1" 2>&1 || true
}

[ -s "$password_file" ] || { log "missing $password_file; rerun install.sh"; exit 1; }

case "${1:-}" in
  --check | --check-docker)
    out="$(backup_query "SHOW GRANTS")"
    code="${out##*$'\n'}"
    body="${out%$'\n'*}"
    body="${body%$'\n'}"
    [ "$code" = 200 ] || { log "cliproxy_backup cannot query ClickHouse at $url (HTTP $code): $body"; exit 1; }
    [[ "$body" == *"GRANT BACKUP ON cliproxy_log.*"* ]] || { log "cliproxy_backup lacks BACKUP on cliproxy_log; rerun install.sh: $body"; exit 1; }
    # GRANT DISK ON *.* is how ClickHouse shows GRANT WRITE ON DISK.
    [[ "$body" == *"GRANT DISK ON *.*"* ]] || { log "cliproxy_backup lacks WRITE ON DISK (GRANT DISK ON *.*); rerun install.sh: $body"; exit 1; }
    log "ClickHouse at $url accepts cliproxy_backup"
    exit 0
    ;;
esac

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

log "creating ClickHouse backup"
out="$(backup_query "BACKUP DATABASE cliproxy_log TO Disk('backups', '$backup_name') FORMAT TSV")"
code="${out##*$'\n'}"
body="${out%$'\n'*}"
body="${body%$'\n'}"
[ "$code" = 200 ] || { log "BACKUP DATABASE failed (HTTP $code): $body"; exit 1; }
status="$(cut -f2 <<<"$body")"
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
