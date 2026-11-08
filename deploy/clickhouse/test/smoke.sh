#!/usr/bin/env bash
# End-to-end check of an installed cliproxy-log ClickHouse (run install.sh first).
# Inserts rows into cliproxy_log.requests, so run it only on a throwaway install
# such as CI, never on the real one.
#
# Checks: the table matches the plugin contract, the writer can insert rows that
# omit optional fields, the reader can search but not write, nobody gets in without
# a password, the port is loopback-only, and (when restic is installed) a backup
# uploads and restores.
#
# Usage: deploy/clickhouse/test/smoke.sh [--with-backup]
set -euo pipefail

config_dir="${CLIPROXY_LOG_CONFIG_DIR:-$HOME/.config/cliproxy-log}"
data_dir="${CLIPROXY_LOG_DATA_DIR:-$HOME/.local/share/cliproxy-log}"
port="${CLIPROXY_LOG_HTTP_PORT:-8123}"
container=cliproxy-clickhouse
with_backup=0
[ "${1:-}" = --with-backup ] && with_backup=1

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}
pass() { printf 'ok: %s\n' "$*"; }

# Prints the response body, then the HTTP status on its own last line.
http() {
  local user="$1" query="$2" body="${3:-}"
  curl -sS --max-time 60 -o - -w '\n%{http_code}' --url-query "query=$query" \
    -H @<(printf 'X-ClickHouse-User: %s\nX-ClickHouse-Key: %s\n' "cliproxy_$user" "$(cat "$config_dir/$user-password")") \
    --data-binary "$body" "http://127.0.0.1:$port/"
}
ok() {
  local out
  out="$(http "$@")"
  [ "${out##*$'\n'}" = 200 ] || fail "$1: '$2' failed: ${out%$'\n'*}"
  printf '%s' "${out%$'\n'*}"
}
denied() {
  local out
  out="$(http "$@")"
  [ "${out##*$'\n'}" != 200 ] || fail "$1 was allowed: $2"
  pass "$1 denied: $2"
}
admin() {
  CLICKHOUSE_PASSWORD="$(cat "$config_dir/admin-password")" \
    docker exec -i -e CLICKHOUSE_USER=cliproxy_admin -e CLICKHOUSE_PASSWORD "$container" \
    clickhouse-client "$@"
}

# --- Contract: column names and types, in order --------------------------------
expected="$(
  cat <<'EOF'
request_id	String
trace_id	String
started_at	DateTime64(3, 'UTC')
completed_at	DateTime64(3, 'UTC')
duration_ms	UInt32
machine	LowCardinality(String)
client_key_id	LowCardinality(String)
endpoint	LowCardinality(String)
transport	LowCardinality(String)
source_format	LowCardinality(String)
target_format	LowCardinality(String)
provider	LowCardinality(String)
account	LowCardinality(String)
auth_id	String
requested_model	LowCardinality(String)
served_model	LowCardinality(String)
response_model	LowCardinality(String)
session_id	String
stream	Bool
status_code	UInt16
outcome	LowCardinality(String)
error	String
input_tokens	UInt64
output_tokens	UInt64
cache_read_tokens	UInt64
cache_write_tokens	UInt64
reasoning_tokens	UInt64
reasoning_effort	LowCardinality(String)
service_tier	LowCardinality(String)
request_headers	Map(String, String)
response_headers	Map(String, String)
client_request	String
upstream_request	String
response	String
response_kind	LowCardinality(String)
truncated	Bool
capture_errors	Array(String)
plugin_version	LowCardinality(String)
proxy_version	LowCardinality(String)
EOF
)"
actual="$(ok reader "DESCRIBE TABLE cliproxy_log.requests FORMAT TSVRaw" | cut -f1,2)"
[ "$actual" = "$expected" ] || fail "columns differ from the contract:$(diff <(echo "$expected") <(echo "$actual") || true)"
pass "table matches the plugin contract"

# --- Writer ------------------------------------------------------------------------
rows="$(
  cat <<'EOF'
{"request_id":"smoke-1","started_at":"2026-11-08 10:00:00.000","completed_at":"2026-11-08 10:00:02.500","duration_ms":2500,"machine":"bigbook","session_id":"s1","endpoint":"/v1/messages","provider":"claude","account":"work","requested_model":"claude-opus-4-5","stream":true,"status_code":200,"outcome":"ok","input_tokens":120000,"output_tokens":800,"cache_read_tokens":110000,"request_headers":{"user-agent":"claude-cli/2.0"},"client_request":"{\"messages\":[{\"role\":\"user\",\"text\":\"where is the zebrafinch config\"}]}","response":"event: message_start","response_kind":"sse_raw","capture_errors":[]}
{"request_id":"smoke-2","started_at":"2026-11-08 10:01:00.000","machine":"bigbook","session_id":"s1","client_request":"{\"messages\":[{\"role\":\"user\",\"text\":\"where is the zebrafinch config\"},{\"role\":\"assistant\",\"text\":\"in config.yaml\"}]}","input_tokens":121000,"output_tokens":300}
{"request_id":"smoke-3","session_id":"s2","client_request":"unrelated"}
EOF
)"
ok writer "INSERT INTO cliproxy_log.requests FORMAT JSONEachRow" "$rows" >/dev/null
pass "writer inserted rows that omit optional fields"
denied writer "DROP TABLE cliproxy_log.requests"
denied writer "ALTER TABLE cliproxy_log.requests DELETE WHERE 1"

# --- Reader ------------------------------------------------------------------------
[ "$(ok reader "SELECT count() FROM cliproxy_log.requests WHERE request_id LIKE 'smoke-%'")" = 3 ] || fail "reader did not see 3 rows"
pass "reader sees the inserted rows"
got="$(ok reader "SELECT machine, response_kind, transport, stream, truncated FROM cliproxy_log.requests WHERE request_id = 'smoke-3' FORMAT TSV")"
[ "$got" = "$(printf 'unknown\tnone\thttp\tfalse\tfalse')" ] || fail "unexpected defaults: $got"
pass "defaults applied for omitted fields"
got="$(ok reader "SELECT groupArray(request_id) FROM (SELECT request_id FROM cliproxy_log.requests WHERE client_request ILIKE '%ZebraFinch%' ORDER BY request_id)")"
[ "$got" = "['smoke-1','smoke-2']" ] || fail "substring search returned $got"
pass "substring search finds the matching requests"
denied reader "INSERT INTO cliproxy_log.requests (request_id) VALUES ('reader')"
denied reader "DROP TABLE cliproxy_log.requests"
denied reader "CREATE TABLE cliproxy_log.x (a UInt8) ENGINE = Memory"
denied reader "SELECT 1 SETTINGS max_execution_time = 100000"
denied reader "SELECT 1 SETTINGS max_execution_time = 0"
denied reader "SELECT 1 SETTINGS max_memory_usage = 0"
denied reader "SELECT 1 SETTINGS readonly = 0"

# --- Network -------------------------------------------------------------------------
anon="$(curl -s --max-time 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/?query=SELECT+1" || true)"
[ "$anon" != 200 ] || fail "unauthenticated query succeeded"
pass "no passwordless access"
bindings="$(docker port "$container")"
[ "$bindings" = "8123/tcp -> 127.0.0.1:$port" ] || fail "unexpected port bindings: $bindings"
pass "published on 127.0.0.1:$port only"

# --- Backup and restore ------------------------------------------------------------------
if [ "$with_backup" = 1 ]; then
  repo="$(mktemp -d)/restic"
  CLIPROXY_LOG_RESTIC_REPOSITORY="$repo" "$HOME/.local/bin/cliproxy-log-backup"
  [ ! -e "$data_dir/backups/cliproxy_log" ] || fail "backup left its local copy behind"
  pass "backup uploaded and cleaned up"

  restore_dir="$data_dir/backups/restore-test"
  RESTIC_PASSWORD_FILE="$config_dir/restic-password" restic --repo "$repo" restore latest --target "$restore_dir"
  marker="$(cd "$restore_dir" && find . -name .backup | head -n1)"
  [ -n "$marker" ] || fail "restored snapshot has no ClickHouse backup"
  backup_path="$(dirname "$marker")"
  admin -q "RESTORE DATABASE cliproxy_log AS cliproxy_log_restored FROM Disk('backups', 'restore-test/${backup_path#./}')" >/dev/null
  n="$(admin -q "SELECT count() FROM cliproxy_log_restored.requests WHERE request_id LIKE 'smoke-%'")"
  admin -q "DROP DATABASE cliproxy_log_restored SYNC"
  rm -rf "$restore_dir"
  [ "$n" = 3 ] || fail "restored database has $n smoke rows, expected 3"
  pass "restore from restic brings the rows back"
fi

echo "all checks passed"
