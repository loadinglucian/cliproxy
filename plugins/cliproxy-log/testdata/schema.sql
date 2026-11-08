-- Test-only DDL written from the cliproxy_log.requests contract. The deployed schema
-- lives in deploy/clickhouse/schema/001_requests.sql; the e2e test prefers that file
-- when it exists and falls back to this one.
CREATE DATABASE IF NOT EXISTS cliproxy_log;

CREATE TABLE IF NOT EXISTS cliproxy_log.requests
(
    request_id String,
    trace_id String,
    started_at DateTime64(3, 'UTC'),
    completed_at DateTime64(3, 'UTC'),
    duration_ms UInt32,
    machine LowCardinality(String),
    client_key_id LowCardinality(String),
    endpoint LowCardinality(String),
    transport LowCardinality(String),
    source_format LowCardinality(String),
    target_format LowCardinality(String),
    provider LowCardinality(String),
    account LowCardinality(String),
    auth_id String,
    requested_model LowCardinality(String),
    served_model LowCardinality(String),
    response_model LowCardinality(String),
    session_id String,
    stream Bool,
    status_code UInt16,
    outcome LowCardinality(String),
    error String,
    input_tokens UInt64,
    output_tokens UInt64,
    cache_read_tokens UInt64,
    cache_write_tokens UInt64,
    reasoning_tokens UInt64,
    reasoning_effort LowCardinality(String),
    service_tier LowCardinality(String),
    request_headers Map(String, String),
    response_headers Map(String, String),
    client_request String,
    upstream_request String,
    response String,
    response_kind LowCardinality(String),
    truncated Bool,
    capture_errors Array(String),
    plugin_version LowCardinality(String),
    proxy_version LowCardinality(String)
)
ENGINE = MergeTree
ORDER BY (started_at, request_id);
