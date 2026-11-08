# cliproxy-log

A CLIProxyAPI plugin that writes one row per model request into the ClickHouse table `cliproxy_log.requests`: who sent it, which account served it, tokens, timing, and the full request and response bodies. It is a C-ABI shared library (ABI v1) loaded by the proxy with `plugins.enabled: true`. It only observes: every hook returns "no change", so requests and responses pass through untouched.

## What it captures

| Captured | Not captured |
| --- | --- |
| Claude `/v1/messages`, OpenAI/Codex `/v1/responses`, Chat Completions and other model calls that reach the proxy's execution path | `/v1/messages/count_tokens` (skipped on purpose) |
| Streaming and non-streaming responses | Model lists (`/v1/models`), management API calls |
| Codex Responses over a client WebSocket, one row per turn | Requests rejected before execution: bad client key (401), unknown model |

Per request it correlates these host callbacks by `RequestID`:

1. `request.intercept_before`: the client's body and headers (`client_request`, `request_headers`, client key).
2. `request.intercept_after`: the body after credential selection (`upstream_request`), the target format, and the served model. On retries the last attempt wins.
3. `response.intercept_after` (non-streaming body) or `response.intercept_stream_chunk` (stream chunks), plus `websocket.response_event` when the upstream connection is a WebSocket.
4. `usage.handle`: provider, account, credential, session, tokens. Usage records carry their own per-attempt ID, so they are matched by `TraceID` (the inbound HTTP request) instead; see [Known limits](#known-limits).
5. `request.complete`: outcome, status, error, start and end times. The row is written when usage has arrived, or 5 s after completion without it.

Requests that never complete (for example the proxy lost track of them) are written with outcome `abandoned` after 30 minutes of inactivity.

### Responses

- Non-streaming JSON is stored as is, `response_kind = 'json'`.
- Anthropic SSE streams are rebuilt into the final Messages object: text, thinking, signature and citation deltas are concatenated, `tool_use` input JSON is assembled and parsed, and `message_delta` stop reason and usage are merged.
- OpenAI Responses streams store the `response` object from the last `response.completed`, `response.failed`, or `response.incomplete` event.
- Anything else (Chat Completions streams, a stream cut before its terminal event) stores the raw stream text with `response_kind = 'sse_raw'`.
- No response (failed or canceled before any output) gives `response_kind = 'none'`.
- Parse problems go to `capture_errors` as fixed descriptions; they never fail the row.

## Build

From the repository root (CGO required; Go comes from mise):

```bash
mise exec -- go build -buildmode=c-shared -o cliproxy-log.so ./plugins/cliproxy-log
rm -f cliproxy-log.h
```

On macOS name the output `cliproxy-log.dylib`. The file name is the plugin ID, so it must be `cliproxy-log.so` (or `cliproxy-log-v<version>.so`). The plugin is a package in the proxy module, not a separate module, so it always builds against the SDK of the proxy it ships with.

`proxy_version` is the proxy module version recorded in the plugin's build info, which matches the proxy when both come from the same release; set `-ldflags "-X main.proxyVersion=8.0.60"` to override it. `-X main.pluginVersion=...` overrides `plugin_version`.

New files under `plugins/` need `git add -f`, because upstream's `.gitignore` ignores `plugins/*`. Files already tracked are unaffected.

## Install

1. Put `cliproxy-log.so` in `<plugins.dir>/linux/amd64/` or `<plugins.dir>/`. A relative `plugins.dir` resolves against the proxy's working directory (on ThinkPad the systemd unit runs in `~/.cli-proxy-api`), so use an absolute path.
2. Create the ClickHouse schema from `deploy/clickhouse/`.
3. Add the config below and save. The proxy picks it up without a restart (see [Reload or restart](#reload-or-restart)).

## Configuration

```yaml
plugins:
  enabled: true
  dir: "/home/loadinglucian/.local/share/cliproxyapi/plugins"
  configs:
    cliproxy-log:
      enabled: true
      priority: 0
      clickhouse-url: "http://127.0.0.1:8123"
      clickhouse-database: "cliproxy_log"
      clickhouse-user: "cliproxy_writer"
      clickhouse-password-file: "/home/loadinglucian/.config/cliproxyapi/clickhouse-writer.password"
      spool-dir: "/home/loadinglucian/.local/state/cliproxy-log/spool"
      spool-max-bytes: 5368709120
      max-body-bytes: 33554432
      machines:
        "<sha256 hex of Bigbook's client key>": "bigbook"
        "<sha256 hex of Workbook's client key>": "workbook"
        "<sha256 hex of ThinkPad's client key>": "thinkpad"
```

| Key | Default | Meaning |
| --- | --- | --- |
| `clickhouse-url` | `http://127.0.0.1:8123` | ClickHouse HTTP endpoint. Credentials in the URL are rejected. |
| `clickhouse-database` | `cliproxy_log` | Database holding `requests`. |
| `clickhouse-user` | `cliproxy_writer` | Sent as `X-ClickHouse-User`. |
| `clickhouse-password-file` | empty (no password) | Read before every insert, so rotating the file needs no reload. |
| `spool-dir` | `cliproxy-log-spool` (relative to the proxy's working directory) | Where unsent batches wait. |
| `spool-max-bytes` | 5 GiB | Spool cap; batches past it are dropped and counted. |
| `max-body-bytes` | 32 MiB | Cap per captured body (client request, upstream request, response or raw stream). Longer bodies are cut and the row gets `truncated = true`. |
| `machines` | empty | sha256 hex of a client API key mapped to a machine name. |

`enabled` and `priority` belong to the host. An invalid config (bad URL, malformed `machines` key) fails `plugin.register`, and the host logs the reason and leaves the plugin inactive.

### Machine attribution

The plugin sees the raw client key in two places: the inbound `Authorization: Bearer` / `x-api-key` header in `request.intercept_before`, and `UsageRecord.APIKey`, which is the authenticated principal (for the built-in `access.api-keys` provider that is the key itself). `CallerScope` is a salted, irreversible hash (`sha256("cli-proxy-api:caller-scope:v1\0" + key)`) and is not used.

The plugin hashes the key immediately and keeps only the hash. `client_key_id` is the first 12 hex characters of `sha256(key)`, and `machine` is the `machines` entry for the full hash, or `unknown`. The usage record's key wins over the header when both exist. The config therefore holds hashes, never keys. Compute one with:

```bash
printf %s "$CLIENT_KEY" | sha256sum | cut -d' ' -f1
```

## Columns

All contract columns of `cliproxy_log.requests` are written. Notable sources:

| Column | Source |
| --- | --- |
| `request_id`, `trace_id` | Host execution ID; inbound HTTP request ID (shared by all turns of one WebSocket connection). |
| `started_at`, `completed_at`, `duration_ms` | `request.complete` times; for `abandoned` rows, first and last event seen. |
| `endpoint` | Route path, for example `/v1/messages`. |
| `transport` | `websocket` when the client request was a WebSocket upgrade, else `http`. |
| `source_format`, `target_format` | Client protocol (`claude`, `openai-response`, ...) and upstream protocol after credential selection. |
| `provider`, `auth_id`, `account` | Usage record (or WebSocket event). `account` is the OAuth account email, or the credential label from WebSocket events. It is empty otherwise: the usage "source" of an API-key credential, or of an OAuth credential without an email, is an upstream or client key. |
| `requested_model`, `served_model`, `response_model` | Client's model; model sent upstream; `model` field in the reply. |
| `session_id` | Usage record. |
| `reasoning_effort`, `service_tier` | Usage record, falling back to request metadata. |
| `status_code`, `outcome`, `error` | `request.complete`; `outcome` is `succeeded`, `failed`, `rejected`, `canceled`, or `abandoned`. |
| token columns | Usage record; without one, the `usage` object in the response body. `cache_read_tokens` falls back to the total cached count. |
| `upstream_request` | The payload after credential selection and model rewriting, still in the client's protocol. The proxy translates it to the upstream protocol inside the executor, after the last hook a plugin can see. |
| `response_headers` | Upstream response headers from the response or stream-init hook. |

## Privacy

- Bodies are stored in full up to `max-body-bytes`: prompts, files, tool output, model replies. Treat the database as sensitive.
- Headers are dropped when named `authorization`, `x-api-key`, `proxy-authorization`, `cookie`, or `set-cookie`, or when the name contains `token`, `secret`, `key`, or `credential`. Kept despite the word filter: `anthropic-version`, `anthropic-beta`, `idempotency-key`, and rate-limit headers starting `anthropic-ratelimit-` or `x-ratelimit-` (counts, not credentials).
- No raw client key is stored or logged; only its sha256 is held in memory and its first 12 hex characters are stored.
- Log lines carry counters, error codes, and row counts only. ClickHouse error bodies are reduced to `Code: N` because they can quote row data.

## Failure behavior

Logging never blocks or fails a request:

- Hooks copy the payload, queue it, and return at once. Decoding, correlation, and reassembly run on one background goroutine. When the queue holds more than 512 MiB or 65,536 events, new events are dropped and counted (`events_dropped`). At most 10,000 requests are tracked at once.
- Every exported entry point and the background goroutines recover from panics (`panics` counter) and return "no change" to the host.
- A writer goroutine sends batches every 2 s or at 500 rows (or 16 MiB) as gzip `INSERT INTO cliproxy_log.requests FORMAT JSONEachRow` over HTTP.
- When an insert fails, the batch is written to `spool-dir` as a gzip file and the writer backs off (1 s doubling to 60 s). During backoff new batches go straight to the spool. After the backoff the oldest spool file is the health probe; once it succeeds, the spool is replayed oldest first, 20 files per cycle. Spool files survive restarts. Each is fsynced before it is renamed into place, and a file that is not a complete gzip stream (cut short by a power loss) is dropped and counted on replay instead of blocking the files behind it.
- Past `spool-max-bytes`, batches are dropped and counted (`rows_dropped`). An HTTP 400 from ClickHouse means the batch itself is malformed, so it is dropped and counted (`rows_rejected`) instead of retried forever.
- On shutdown the plugin writes every in-flight request (incomplete ones as `abandoned`) and makes one last insert attempt, spooling on failure. If ClickHouse has not answered within 10 s, the pending insert is canceled and its rows go to the spool too.
- Inserts ask ClickHouse to answer only after the insert finishes (`wait_end_of_query=1`). An insert counts as written only when ClickHouse answers HTTP 200 without an `X-ClickHouse-Exception-Code` header and the reply is read in full; a cut-off reply is retried from the spool, which can duplicate that batch.
- Once a minute, if anything changed, the host log gets `cliproxy-log stats` with `rows_written`, `rows_spooled`, `rows_dropped`, `rows_rejected`, `events_dropped`, `capture_errors`, `abandoned`, `panics`, and `spool_bytes` (warn level when anything was lost).

## Reload or restart

Verified against the proxy's file watcher and `internal/pluginhost/host.go` (`ApplyConfig`):

- **Enabling, disabling, or editing `plugins.configs.cliproxy-log`: no restart.** Saving `config.yaml` triggers `ApplyConfig`, which loads the plugin and calls `plugin.register`, calls `plugin.reconfigure` with the new YAML, or unloads it. The plugin applies new settings (including `machines` and the ClickHouse target) on the fly.
- **Replacing the `.so` at the same path: needs a restart.** The host compares paths only, so an overwritten file is ignored until the next start.
- **A new versioned file (`cliproxy-log-v0.2.0.so`) plus a config save hot-reloads**, but Go shared libraries cannot be unloaded: the old copy stays mapped and retired until the plugin is disabled or the proxy exits, and requests in flight during the swap end up as `abandoned` rows. Restart the proxy for binary updates.

## WebSocket turns

Codex WebSocket turns are visible to the same hooks as HTTP requests. `sdk/api/handlers/openai/openai_responses_websocket.go` runs every `response.create` turn through `ExecuteStreamWithAuthManager`, which calls the request interceptors, the stream chunk interceptor, the usage observer, and `request.complete` with a fresh `RequestID`. The WebSocket response observer only fires when the upstream connection is itself a WebSocket (Codex OAuth or `websockets: true` keys); the plugin uses those events as a fallback response source and for the credential label. The e2e test covers a client WebSocket turn against an HTTP upstream and asserts its row (`transport = 'websocket'`, reassembled response, tokens).

## Known limits

- All turns on one WebSocket connection share a `TraceID`. Turns run one at a time, so a usage record goes to the newest turn first seen no later than the record's attempt started (`RequestedAt`); if that turn already has successful usage (an image tool's side-model record), the record is ignored. A late record therefore never lands on another turn. This compares wall-clock times, so a backward clock step of the proxy host between a turn's first hook and its usage reporter can leave that turn with tokens parsed from the response body instead.
- A stream cut at `max-body-bytes` reassembles only what was captured; the row is marked `truncated`.
- With Codex response steering enabled, several client inputs can share one execution; they are logged as one row with the last upstream body.
- Hooks see the client-protocol payload, not the bytes the executor finally sends upstream (see `upstream_request`).

## Tests

```bash
mise exec -- go test -race ./plugins/cliproxy-log/...
```

Unit tests cover correlation (including late usage, retries, and shared WebSocket traces), redaction, Anthropic and Responses stream reassembly from realistic fixtures in `testdata/`, non-stream capture, truncation, spool and replay, drop accounting, and panic safety.

The end-to-end test builds the proxy and the plugin, starts the proxy with a fake Claude and Codex upstream, sends non-streaming, streaming, `count_tokens`, model-list, and WebSocket requests, and checks the rows in a real ClickHouse:

```bash
docker run -d --rm -p 127.0.0.1:18123:8123 -e CLICKHOUSE_SKIP_USER_SETUP=1 clickhouse/clickhouse-server:26.8.34.13@sha256:8b28eb09479c36c4533eec7a6268917d04ba3a6fd90e3f4205f70e761b6d7779
CLIPROXY_LOG_E2E_CLICKHOUSE_URL=http://127.0.0.1:18123 mise exec -- go test -tags e2e -v ./plugins/cliproxy-log/e2e
```

It applies `deploy/clickhouse/schema/001_requests.sql` when present and otherwise `testdata/schema.sql`, a test-only copy of the column contract. CI runs both suites in `.github/workflows/fork-plugin.yml`.
