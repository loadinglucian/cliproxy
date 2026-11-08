-- One row per model request proxied by CLIProxyAPI, written by the cliproxy-log
-- plugin when the request completes. Column names and types are a contract with
-- the plugin (plugins/cliproxy-log); change them only together with it.
--
-- Safe to run repeatedly: every statement is IF NOT EXISTS.
--
-- Storage design (measured with bench/bench.sh, see README.md):
--   * ORDER BY (machine, session_id, started_at): a Claude Code request resends the
--     whole conversation, so consecutive requests of one session are ~95% identical.
--     Sorting them next to each other lets ZSTD encode each request as a reference to
--     the previous one. Ordering by time interleaves concurrent sessions and loses that.
--   * Body columns use 16-32 MiB compression blocks instead of the 64 KiB-1 MiB
--     default, so one block holds dozens of turns of a session rather than one or two.
--     Reading a single row then decompresses up to 32 MiB, which costs tens of
--     milliseconds and is irrelevant for history search.
--   * Request bodies use ZSTD long-range mode (window_log 24, a 16 MiB window) so a
--     multi-megabyte request from a long-context session can still reference the
--     previous turn; plain ZSTD(3) only looks back 2 MiB.
--   * Monthly partitions keep parts small enough to merge and make it cheap to drop
--     or archive old months later. Data is kept forever for now (no TTL).
--   * Text indexes (GA in 26.x) on client_request and response turn substring and
--     token searches into posting-list lookups instead of full scans. The latest
--     client_request of a session contains every earlier user and assistant message,
--     so these two columns cover the whole history; upstream_request is the translated
--     copy of client_request and is not indexed.
--   * The minmax index on started_at lets time-range queries skip granules even
--     though time is not the leading sort key; a session spans minutes to hours.

CREATE DATABASE IF NOT EXISTS cliproxy_log;

CREATE TABLE IF NOT EXISTS cliproxy_log.requests
(
    request_id         String DEFAULT toString(generateUUIDv4()),
    trace_id           String DEFAULT '',
    started_at         DateTime64(3, 'UTC') DEFAULT now64(3, 'UTC') CODEC(Delta, ZSTD(1)),
    completed_at       DateTime64(3, 'UTC') DEFAULT now64(3, 'UTC') CODEC(Delta, ZSTD(1)),
    duration_ms        UInt32 DEFAULT 0,

    machine            LowCardinality(String) DEFAULT 'unknown',
    client_key_id      LowCardinality(String) DEFAULT '',

    endpoint           LowCardinality(String) DEFAULT '',
    transport          LowCardinality(String) DEFAULT 'http',
    source_format      LowCardinality(String) DEFAULT '',
    target_format      LowCardinality(String) DEFAULT '',

    provider           LowCardinality(String) DEFAULT '',
    account            LowCardinality(String) DEFAULT '',
    auth_id            String DEFAULT '',

    requested_model    LowCardinality(String) DEFAULT '',
    served_model       LowCardinality(String) DEFAULT '',
    response_model     LowCardinality(String) DEFAULT '',

    session_id         String DEFAULT '',
    stream             Bool DEFAULT false,
    status_code        UInt16 DEFAULT 0,
    outcome            LowCardinality(String) DEFAULT '',
    error              String DEFAULT '' CODEC(ZSTD(3)),

    input_tokens       UInt64 DEFAULT 0,
    output_tokens      UInt64 DEFAULT 0,
    cache_read_tokens  UInt64 DEFAULT 0,
    cache_write_tokens UInt64 DEFAULT 0,
    reasoning_tokens   UInt64 DEFAULT 0,

    reasoning_effort   LowCardinality(String) DEFAULT '',
    service_tier       LowCardinality(String) DEFAULT '',

    request_headers    Map(String, String) DEFAULT map() CODEC(ZSTD(3)),
    response_headers   Map(String, String) DEFAULT map() CODEC(ZSTD(3)),

    client_request     String DEFAULT '' CODEC(ZSTD(3, 24))
        SETTINGS (min_compress_block_size = 16777216, max_compress_block_size = 33554432),
    upstream_request   String DEFAULT '' CODEC(ZSTD(3, 24))
        SETTINGS (min_compress_block_size = 16777216, max_compress_block_size = 33554432),
    response           String DEFAULT '' CODEC(ZSTD(3))
        SETTINGS (min_compress_block_size = 16777216, max_compress_block_size = 33554432),
    response_kind      LowCardinality(String) DEFAULT 'none',

    truncated          Bool DEFAULT false,
    capture_errors     Array(String) DEFAULT [],
    plugin_version     LowCardinality(String) DEFAULT '',
    proxy_version      LowCardinality(String) DEFAULT '',

    INDEX idx_started_at     started_at     TYPE minmax GRANULARITY 1,
    INDEX idx_request_id     request_id     TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_client_request client_request TYPE text(tokenizer = splitByNonAlpha),
    INDEX idx_response       response       TYPE text(tokenizer = splitByNonAlpha)
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(started_at)
ORDER BY (machine, session_id, started_at);
