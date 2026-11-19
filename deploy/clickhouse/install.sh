#!/usr/bin/env bash
# Installs or updates the cliproxy-log ClickHouse on this computer. Safe to rerun:
# existing passwords, data, and backups are kept, and the schema is applied with
# IF NOT EXISTS. It never touches CLIProxyAPI itself. Run it as yourself, never
# with sudo: it uses Docker directly when you can, otherwise through sudo for the
# Docker commands only, so every file it writes stays yours.
#
# Usage: deploy/clickhouse/install.sh [--no-timer]
#   --no-timer  skip installing and enabling the nightly backup systemd timer
#
# Environment overrides (defaults in brackets):
#   CLIPROXY_LOG_CONFIG_DIR  passwords and rendered user config [~/.config/cliproxy-log]
#   CLIPROXY_LOG_DATA_DIR    database files and backup staging  [~/.local/share/cliproxy-log]
#   CLIPROXY_LOG_HTTP_PORT   loopback HTTP port                 [8123]
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
install_timer=1
for arg in "$@"; do
  case "$arg" in
    --no-timer) install_timer=0 ;;
    -h | --help)
      sed -n '2,15p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown option: $arg" >&2
      exit 2
      ;;
  esac
done

export CLIPROXY_LOG_CONFIG_DIR="${CLIPROXY_LOG_CONFIG_DIR:-$HOME/.config/cliproxy-log}"
export CLIPROXY_LOG_DATA_DIR="${CLIPROXY_LOG_DATA_DIR:-$HOME/.local/share/cliproxy-log}"
export CLIPROXY_LOG_HTTP_PORT="${CLIPROXY_LOG_HTTP_PORT:-8123}"
CLIPROXY_LOG_UID="$(id -u)"
CLIPROXY_LOG_GID="$(id -g)"
export CLIPROXY_LOG_UID CLIPROXY_LOG_GID
container=cliproxy-clickhouse
config_dir="$CLIPROXY_LOG_CONFIG_DIR"
data_dir="$CLIPROXY_LOG_DATA_DIR"

step() { printf '==> %s\n' "$*"; }
die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# --- 1. Preflight -------------------------------------------------------------
step "checking prerequisites"
[ "$(id -u)" != 0 ] || die "run this as the user who owns the install, not root; it uses sudo for Docker when it needs to"
for cmd in docker openssl sha256sum curl; do
  command -v "$cmd" >/dev/null || die "$cmd is not installed"
done
# Docker is root-equivalent, so the user may have no direct access to it (not in the
# docker group). Then every docker command below runs through sudo, which asks for
# the password once (sudo reads it from the terminal, never stdin). sudo resets the
# environment, so the variables docker-compose.yml reads, and the admin password for
# admin_client, are passed by name with --preserve-env: never on a command line,
# which ps and sudo's log would show. HOME stays root's, so the Docker CLI never
# creates root-owned files in this user's home; every path compose needs is exported
# as an absolute path (step 2) before compose runs, and the container still runs as
# this user's uid.
docker_sudo=()
if ! command docker info >/dev/null 2>&1; then
  command -v sudo >/dev/null || die "cannot reach the Docker daemon as $(id -un), and sudo is not installed. Start it with 'systemctl enable --now docker' as root."
  step "Docker needs root here; running docker commands through sudo"
  keep=CLIPROXY_LOG_CONFIG_DIR,CLIPROXY_LOG_DATA_DIR,CLIPROXY_LOG_HTTP_PORT,CLIPROXY_LOG_UID,CLIPROXY_LOG_GID,CLICKHOUSE_PASSWORD
  docker_sudo=(sudo "--preserve-env=$keep" --)
  "${docker_sudo[@]}" docker info >/dev/null 2>&1 ||
    die "cannot reach the Docker daemon through sudo. Start it with 'sudo systemctl enable --now docker'."
fi
docker() {
  if [ "${#docker_sudo[@]}" -gt 0 ]; then
    "${docker_sudo[@]}" docker "$@"
  else
    command docker "$@"
  fi
}
docker compose version >/dev/null 2>&1 || die "the docker compose plugin is not installed"
# With only docker.socket enabled, dockerd (and with it this container) starts at boot
# only once something runs a docker command, so the plugin would spool until then.
if command -v systemctl >/dev/null && [ "$(systemctl is-enabled docker.service 2>/dev/null)" != enabled ]; then
  echo "warning: docker.service is not enabled, so ClickHouse will not start at boot; run 'sudo systemctl enable docker'" >&2
fi

# --- 2. Directories -------------------------------------------------------------
# ClickHouse compresses its own files, so btrfs copy-on-write and compression only
# add fragmentation and CPU. +C must be set while the directory is still empty;
# new files inherit it.
make_data_dir() {
  local dir="$1"
  mkdir -p "$dir"
  if [ "$(uname -s)" = Linux ] && [ "$(stat -f -c %T "$dir")" = btrfs ] && [ -z "$(ls -A "$dir")" ]; then
    chattr +C "$dir"
  fi
}

step "creating directories"
mkdir -p "$config_dir/users.d"
chmod 0700 "$config_dir" "$config_dir/users.d"
mkdir -p "$data_dir"
chmod 0700 "$data_dir"
make_data_dir "$data_dir/clickhouse"
make_data_dir "$data_dir/backups"
# Absolute paths from here on: docker compose resolves relative ones against its own
# directory, and the backup timer stores them (step 8).
config_dir="$(cd "$config_dir" && pwd)"
data_dir="$(cd "$data_dir" && pwd)"
export CLIPROXY_LOG_CONFIG_DIR="$config_dir" CLIPROXY_LOG_DATA_DIR="$data_dir"

# --- 3. Passwords ---------------------------------------------------------------
# Each password is generated once and never printed. Files are 0600 in a 0700 dir
# and hold 64 hex characters without a trailing newline.
ensure_secret() {
  local file="$config_dir/$1"
  if [ ! -s "$file" ]; then
    (
      umask 077
      openssl rand -hex 32 | tr -d '\n' >"$file.tmp"
      mv "$file.tmp" "$file"
    )
    echo "    created $file"
  fi
  chmod 0600 "$file"
}

step "ensuring passwords"
for name in admin writer reader backup restic; do
  ensure_secret "$name-password"
done

# --- 4. User config ---------------------------------------------------------------
sha256_of() { tr -d '\n' <"$config_dir/$1-password" | sha256sum | cut -d' ' -f1; }

step "rendering user config"
users_file="$config_dir/users.d/cliproxy-users.xml"
(
  umask 077
  sed -e "s/@ADMIN_SHA256@/$(sha256_of admin)/" \
    -e "s/@WRITER_SHA256@/$(sha256_of writer)/" \
    -e "s/@READER_SHA256@/$(sha256_of reader)/" \
    -e "s/@BACKUP_SHA256@/$(sha256_of backup)/" \
    "$here/config/users.xml.template" >"$users_file.tmp"
  # ClickHouse reloads users.d on change; replace atomically so it never reads half a file.
  mv "$users_file.tmp" "$users_file"
)

# --- 5. Server --------------------------------------------------------------------
step "starting ClickHouse"
# Compose recreates a running container whose settings differ from this file as
# rendered here: an image change, or a container first started from another checkout
# or with other exports (bind-mount paths are part of the settings). Say so before it
# happens: ClickHouse is down for a few seconds and the plugin spools meanwhile.
plan="$(docker compose -f "$here/docker-compose.yml" up -d --dry-run 2>&1 || true)"
case "$plan" in
  *" Recreate"*) step "container settings changed; recreating ClickHouse (the proxy spools rows meanwhile)" ;;
esac
docker compose -f "$here/docker-compose.yml" up -d --wait --quiet-pull --remove-orphans
# A single-file bind mount keeps the file it saw when the container started, and git
# replaces files on pull, so recreate the container when the server config changed.
if ! docker exec "$container" cat /etc/clickhouse-server/config.d/cliproxy.xml | cmp -s - "$here/config/cliproxy.xml"; then
  step "server config changed; recreating ClickHouse"
  docker compose -f "$here/docker-compose.yml" up -d --wait --force-recreate
fi

# Runs clickhouse-client inside the container as the admin. The password travels
# through the environment (through sudo by name, see step 1), never the command line.
admin_client() {
  CLICKHOUSE_PASSWORD="$(cat "$config_dir/admin-password")" \
    docker exec -i -e CLICKHOUSE_USER=cliproxy_admin -e CLICKHOUSE_PASSWORD "$container" \
    clickhouse-client "$@"
}

# The users file can change on a rerun; wait until the admin can log in with the current password.
for _ in $(seq 30); do
  admin_client -q 'SELECT 1' >/dev/null 2>&1 && break
  sleep 1
done
admin_client -q 'SELECT 1' >/dev/null || die "admin login failed; check 'docker logs $container'"

# --- 6. Schema --------------------------------------------------------------------
step "applying schema"
for sql in "$here"/schema/*.sql; do
  echo "    $(basename "$sql")"
  admin_client --multiquery <"$sql"
done

# --- 7. Verify users over HTTP ------------------------------------------------------
# Sends one query over the published port as the given user and prints the response
# body; credentials go in headers read from a file descriptor, not argv.
http_query() {
  local user="$1" query="$2"
  curl -sS --max-time 30 -o - -w '\n%{http_code}' \
    -H @<(printf 'X-ClickHouse-User: %s\nX-ClickHouse-Key: %s\n' "cliproxy_$user" "$(cat "$config_dir/$user-password")") \
    --data-binary "$query" "http://127.0.0.1:$CLIPROXY_LOG_HTTP_PORT/"
}
expect_ok() {
  local out
  out="$(http_query "$1" "$2")"
  [ "${out##*$'\n'}" = 200 ] || die "$1 could not run '$2': ${out%$'\n'*}"
}
expect_denied() {
  local out
  out="$(http_query "$1" "$2")"
  [ "${out##*$'\n'}" != 200 ] || die "$1 was allowed to run '$2'"
}

step "verifying users"
expect_ok writer "SELECT count() FROM cliproxy_log.requests"
grants="$(http_query writer "SHOW GRANTS")"
case "$grants" in
  *"GRANT SELECT, INSERT ON cliproxy_log.*"* | *"GRANT INSERT, SELECT ON cliproxy_log.*"*) ;;
  *) die "writer lacks INSERT and SELECT on cliproxy_log: $grants" ;;
esac
expect_denied writer "DROP TABLE cliproxy_log.requests"
expect_ok reader "SELECT count() FROM cliproxy_log.requests"
expect_denied reader "INSERT INTO cliproxy_log.requests (request_id) VALUES ('install-check')"
expect_denied reader "SELECT 1 SETTINGS max_execution_time = 100000"
# A new backup-password only reaches ClickHouse when it reloads users.d, a few
# seconds after step 4 rewrote it.
for _ in $(seq 30); do
  [ "$(http_query backup "SELECT 1")" = "$(printf '1\n\n200')" ] && break
  sleep 1
done
grants="$(http_query backup "SHOW GRANTS")"
case "$grants" in
  *"GRANT BACKUP ON cliproxy_log.*"*) ;;
  *) die "backup user lacks BACKUP on cliproxy_log: $grants" ;;
esac
expect_denied backup "SELECT count() FROM cliproxy_log.requests"
expect_denied backup "INSERT INTO cliproxy_log.requests (request_id) VALUES ('install-check')"
expect_denied backup "CREATE TABLE cliproxy_log.install_check (a UInt8) ENGINE = Memory"
anonymous="$(curl -s --max-time 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$CLIPROXY_LOG_HTTP_PORT/?query=SELECT+1" || true)"
[ "$anonymous" != 200 ] || die "an unauthenticated query succeeded; the default user still exists"

# --- 8. Nightly backup ----------------------------------------------------------------
step "installing backup script"
mkdir -p "$HOME/.local/bin"
install -m 0755 "$here/backup.sh" "$HOME/.local/bin/cliproxy-log-backup"

if [ "$install_timer" = 1 ]; then
  step "installing nightly backup timer"
  unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  mkdir -p "$unit_dir"
  install -m 0644 "$here/systemd/cliproxy-log-backup.service" "$here/systemd/cliproxy-log-backup.timer" "$unit_dir/"
  # backup.sh reads the same path overrides as this script; store the ones used here
  # so the timer backs up this install. systemd expands %, so double it.
  case "$config_dir$data_dir" in
    *[\"\\]* | *$'\n'*) die "paths for the backup timer must not contain quotes, backslashes, or newlines" ;;
  esac
  mkdir -p "$unit_dir/cliproxy-log-backup.service.d"
  printf '[Service]\nEnvironment="CLIPROXY_LOG_CONFIG_DIR=%s" "CLIPROXY_LOG_DATA_DIR=%s"\n' \
    "${config_dir//%/%%}" "${data_dir//%/%%}" >"$unit_dir/cliproxy-log-backup.service.d/paths.conf"
  systemctl --user daemon-reload
  systemctl --user enable --now cliproxy-log-backup.timer
  # Check from where the timer runs, the systemd user manager, that the backup user
  # can log in over HTTP.
  if ! systemd-run --user --wait --quiet --collect "$HOME/.local/bin/cliproxy-log-backup" --check >/dev/null 2>&1; then
    echo "warning: the backup timer cannot reach ClickHouse as cliproxy_backup; see 'journalctl --user -n 20'" >&2
  fi
fi
command -v restic >/dev/null || echo "warning: restic is not installed; install it (sudo pacman -S --needed restic) before the first backup" >&2

cat <<EOF

Done. ClickHouse listens on http://127.0.0.1:$CLIPROXY_LOG_HTTP_PORT (Play UI: /play).
Passwords (never printed) are in $config_dir:
  writer-password  for the cliproxy-log plugin (user cliproxy_writer)
  reader-password  for searching (user cliproxy_reader)
  backup-password  for the nightly backup (user cliproxy_backup, backups only)
  admin-password   for schema changes and restores (user cliproxy_admin, docker exec only)
  restic-password  for the NAS backup repository; save it in Proton Pass now
EOF
