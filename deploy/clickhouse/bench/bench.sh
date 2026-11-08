#!/usr/bin/env bash
# Measures storage and search cost of schema/001_requests.sql on synthetic traffic.
#
# Starts a throwaway ClickHouse container from the image pinned in
# docker-compose.yml, fills it with growing conversations (synthetic.sql), loads
# them into the real schema and into a naive time-ordered baseline, then prints
# compressed sizes and substring-search latencies. Nothing outside the container
# is touched; the container is removed on exit.
#
# Usage: deploy/clickhouse/bench/bench.sh [sessions]
#   sessions  conversations of 40 turns each to generate [48, about 1 GB of bodies]
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
image="$(sed -n 's/^ *image: *//p' "$here/../docker-compose.yml" | head -n1)"
name="cliproxy-log-bench-$$"
sessions="${1:-48}"

cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT

ch() { docker exec -i "$name" clickhouse-client "$@"; }

echo "image: $image"
docker run -d --name "$name" -e CLICKHOUSE_SKIP_USER_SETUP=1 \
  --ulimit nofile=262144:262144 "$image" >/dev/null
for _ in $(seq 60); do
  ch -q 'SELECT 1' >/dev/null 2>&1 && break
  sleep 1
done

echo "generating synthetic traffic..."
ch --param_sessions="$sessions" --queries-file /dev/stdin <"$here/synthetic.sql"
ch --queries-file /dev/stdin <"$here/../schema/001_requests.sql"

# The naive baseline: same columns, default codec (LZ4) and block sizes, time order, no text indexes.
ch --multiquery -q "
CREATE TABLE cliproxy_log.requests_by_time AS cliproxy_log.requests
ENGINE = MergeTree PARTITION BY toYYYYMM(started_at) ORDER BY started_at;
ALTER TABLE cliproxy_log.requests_by_time DROP INDEX idx_client_request, DROP INDEX idx_response;
ALTER TABLE cliproxy_log.requests_by_time MODIFY COLUMN client_request REMOVE CODEC;
ALTER TABLE cliproxy_log.requests_by_time MODIFY COLUMN upstream_request REMOVE CODEC;
ALTER TABLE cliproxy_log.requests_by_time MODIFY COLUMN response REMOVE CODEC;
ALTER TABLE cliproxy_log.requests_by_time MODIFY COLUMN client_request REMOVE SETTINGS;
ALTER TABLE cliproxy_log.requests_by_time MODIFY COLUMN upstream_request REMOVE SETTINGS;
ALTER TABLE cliproxy_log.requests_by_time MODIFY COLUMN response REMOVE SETTINGS;"

for t in requests requests_by_time; do
  ch -q "
  INSERT INTO cliproxy_log.$t (request_id, started_at, completed_at, duration_ms, machine, endpoint, provider,
    requested_model, served_model, response_model, session_id, stream, status_code, outcome,
    input_tokens, output_tokens, client_request, upstream_request, response, response_kind)
  SELECT toString(cityHash64(session_id, started_at)), started_at, started_at + toIntervalSecond(20), 20000, machine,
    '/v1/messages', 'claude', 'claude-opus-4-5', 'claude-opus-4-5', 'claude-opus-4-5-20251101', session_id, true, 200, 'ok',
    intDiv(length(client_request), 4), intDiv(length(response), 40), client_request, upstream_request, response, 'sse_raw'
  FROM bench.src"
  ch -q "OPTIMIZE TABLE cliproxy_log.$t FINAL"
done

echo
echo "== storage (bodies) =="
ch --format PrettyCompactMonoBlock -q "
SELECT table, column,
  formatReadableSize(sum(column_data_uncompressed_bytes)) AS uncompressed,
  formatReadableSize(sum(column_data_compressed_bytes)) AS on_disk,
  round(sum(column_data_uncompressed_bytes) / sum(column_data_compressed_bytes), 1) AS ratio
FROM system.parts_columns
WHERE database = 'cliproxy_log' AND active AND column IN ('client_request', 'upstream_request', 'response')
GROUP BY table, column ORDER BY table, column"

echo "== storage (whole table incl. indexes) =="
ch --format PrettyCompactMonoBlock -q "
SELECT table, sum(rows) AS rows,
  formatReadableSize(sum(data_uncompressed_bytes)) AS uncompressed,
  formatReadableSize(sum(bytes_on_disk)) AS on_disk,
  round(sum(data_uncompressed_bytes) / sum(bytes_on_disk), 1) AS ratio,
  formatReadableSize(sum(secondary_indices_compressed_bytes)) AS skip_indexes
FROM system.parts WHERE database = 'cliproxy_log' AND active GROUP BY table ORDER BY table"

# Each query runs three times; the table shows the fastest run (warm page cache).
queries=(
  "SELECT count() FROM cliproxy_log.TABLE WHERE client_request LIKE '%xylophonequartz%'"
  "SELECT count() FROM cliproxy_log.TABLE WHERE hasToken(client_request, 'xylophonequartz42')"
  "SELECT session_id, max(started_at) FROM cliproxy_log.TABLE WHERE client_request ILIKE '%XYLOPHONEquartz%' GROUP BY session_id"
  "SELECT count() FROM cliproxy_log.TABLE WHERE client_request LIKE '%needle xylophonequartz42 is%'"
)
i=0
for q in "${queries[@]}"; do
  i=$((i + 1))
  for variant in requests requests_by_time requests_noindex; do
    # The query condition cache would let repeat runs skip granules that missed before.
    table="$variant" settings="SETTINGS use_query_condition_cache = 0"
    if [ "$variant" = requests_noindex ]; then
      table=requests
      settings="$settings, use_skip_indexes = 0, query_plan_direct_read_from_text_index = 0"
    fi
    for run in 1 2 3; do
      ch --log_comment "bench q$i $variant $run" -q "${q//TABLE/$table} $settings" >/dev/null
    done
  done
done
ch -q "SYSTEM FLUSH LOGS"
echo "== search latency (best of 3, warm cache) =="
ch --format PrettyCompactMonoBlock -q "
SELECT splitByChar(' ', log_comment)[2] AS q, splitByChar(' ', log_comment)[3] AS variant,
  min(query_duration_ms) AS ms, min(read_rows) AS rows_read, formatReadableSize(min(read_bytes)) AS bytes_read,
  any(substring(query, 1, 90)) AS query
FROM system.query_log
WHERE type = 'QueryFinish' AND log_comment LIKE 'bench q%'
GROUP BY q, variant ORDER BY q, variant"
