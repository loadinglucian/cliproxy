# cliproxy-log ClickHouse

A local ClickHouse database that stores every model request CLIProxyAPI handles, with full request and response bodies, for searching past conversations and analysing usage per machine, model, and account. The cliproxy-log plugin (`plugins/cliproxy-log`) writes one row per request into `cliproxy_log.requests` over HTTP on `127.0.0.1:8123`. Nothing outside the computer can reach it.

| Path | What it is |
| --- | --- |
| `docker-compose.yml` | The server: `clickhouse/clickhouse-server:26.8.34.13` (26.8 LTS), loopback-only, 16 GiB memory cap |
| `config/cliproxy.xml` | Server overrides: console logging, backups disk, system-log retention |
| `config/users.xml.template` | The four users; `install.sh` fills in password hashes |
| `schema/001_requests.sql` | Database and table DDL, safe to rerun |
| `schema/002_request_tail.sql` | `request_tail`, each request's new messages, for search; safe to rerun |
| `install.sh` | Installs or updates everything on this computer |
| `backup.sh`, `systemd/` | Nightly backup to the NAS |
| `bench/` | Storage and search benchmark on synthetic traffic |
| `test/smoke.sh` | End-to-end check used by CI (`.github/workflows/fork-clickhouse.yml`) |

## Install

Prerequisites on ThinkPad (one time, needs sudo):

```sh
sudo systemctl enable --now docker
sudo pacman -S --needed restic
```

Then, from a checkout of this repository, as yourself (never with `sudo`):

```sh
deploy/clickhouse/install.sh
```

Docker is root-equivalent, so on ThinkPad your user is not in the `docker` group. When `docker` does not work as you, `install.sh` runs only its Docker commands through `sudo` and asks for your password (on the terminal, never from stdin); everything else, including every file it writes, runs as you. Where your user can reach Docker directly (CI), it uses Docker as is.

It is safe to rerun after pulling changes. It:

1. Creates `~/.config/cliproxy-log` (0700) and `~/.local/share/cliproxy-log/{clickhouse,backups}`, marking the data directories no-copy-on-write (`chattr +C`) on btrfs while they are still empty. ClickHouse compresses its own files, so btrfs compression and copy-on-write only cost CPU and fragmentation.
2. Generates five passwords once (0600 files, never printed): `admin-password`, `writer-password`, `reader-password`, `backup-password`, `restic-password`. Existing files are kept, so a rerun only adds missing ones.
3. Renders `~/.config/cliproxy-log/users.d/cliproxy-users.xml` with SHA-256 hashes of those passwords.
4. Starts the container, applies every `schema/*.sql`, and checks each user's permissions over HTTP. A running container is recreated when its settings differ from `docker-compose.yml` as `install.sh` renders it (a new image, or a container first started from another checkout or with other exports, whose bind-mount paths differ; the script prints "container settings changed" first), or when `config/cliproxy.xml` changed ("server config changed"). Either way ClickHouse is down for a few seconds, and the proxy spools rows and replays them.
5. Installs `~/.local/bin/cliproxy-log-backup` and enables the `cliproxy-log-backup.timer` user unit with the directories and HTTP port used here (skip with `--no-timer`).

It never touches CLIProxyAPI. **Save `~/.config/cliproxy-log/restic-password` in Proton Pass** (for example as "cliproxy-log restic backup"): without it the NAS backups cannot be decrypted.

The container runs as your user, so all files stay yours. To run `docker compose` by hand, export what `install.sh` exports and pass it through `sudo`, which otherwise resets the environment (and `HOME`, so the default paths would point into `/root`):

```sh
export CLIPROXY_LOG_UID=$(id -u) CLIPROXY_LOG_GID=$(id -g) \
  CLIPROXY_LOG_CONFIG_DIR=~/.config/cliproxy-log CLIPROXY_LOG_DATA_DIR=~/.local/share/cliproxy-log
sudo --preserve-env=CLIPROXY_LOG_UID,CLIPROXY_LOG_GID,CLIPROXY_LOG_CONFIG_DIR,CLIPROXY_LOG_DATA_DIR \
  docker compose -f deploy/clickhouse/docker-compose.yml ps
```

### Upgrading from an install without `cliproxy_backup`

Rerun `install.sh` once from the updated checkout. It creates `backup-password`, adds `cliproxy_backup` to the rendered users file (ClickHouse reloads it without a restart), checks the new user's grants, and installs the new backup script and unit. Then check the backup the way the timer runs it:

```sh
systemd-run --user --wait --pipe --collect ~/.local/bin/cliproxy-log-backup --check
```

To upgrade ClickHouse, change the image tag and digest in `docker-compose.yml` (stay on an LTS line, `.3` or `.8`) and rerun `install.sh`.

## Users

| User | Password file | Can do | Reachable from |
| --- | --- | --- | --- |
| `cliproxy_writer` | `writer-password` | `INSERT`, `SELECT` on `cliproxy_log.*` | `127.0.0.1:8123` |
| `cliproxy_reader` | `reader-password` | `SELECT` on `cliproxy_log.*`; queries stop after 300 s or 8 GiB and the limits cannot be raised or turned off | `127.0.0.1:8123` |
| `cliproxy_backup` | `backup-password` | `BACKUP` on `cliproxy_log.*` and `WRITE ON DISK`, which is all `BACKUP DATABASE ... TO Disk('backups', ...)` needs; no `SELECT`, `INSERT`, DDL, `RESTORE`, or user management | `127.0.0.1:8123` |
| `cliproxy_admin` | `admin-password` | everything except user management (schema changes, restores, system tables) | inside the container only (`docker exec`, which needs root on ThinkPad) |

There is no `default` user and no passwordless access. ClickHouse 26.8 ignores a disk-name filter on `WRITE ON DISK`, so the backup user's grant covers every disk, but `config/cliproxy.xml` allows backups only to the `backups` disk and the user holds nothing else that writes to one; `SHOW GRANTS` lists it as `GRANT DISK ON *.*`. Password files hold 64 hex characters without a trailing newline. Inserts by the writer are batched server-side (`async_insert`) and return once written.

## Query

**Play UI**: open <http://127.0.0.1:8123/play> on ThinkPad, enter `cliproxy_reader` and the contents of `reader-password`. From another computer, tunnel first: `ssh -L 8123:127.0.0.1:8123 thinkpad`.

**HTTP**: the reader needs no Docker access. The credentials go in headers read from a file descriptor, not the command line:

```sh
curl -sS -H @<(printf 'X-ClickHouse-User: cliproxy_reader\nX-ClickHouse-Key: %s\n' "$(cat ~/.config/cliproxy-log/reader-password)") \
  --data-binary "SELECT count() FROM cliproxy_log.requests" http://127.0.0.1:8123/
```

**As the admin** (`clickhouse-client` inside the container), which on ThinkPad needs root because `docker exec` does. The password goes through the environment, by name through `sudo`, never the command line:

```sh
CLICKHOUSE_PASSWORD="$(cat ~/.config/cliproxy-log/admin-password)" sudo --preserve-env=CLICKHOUSE_PASSWORD \
  docker exec -it -e CLICKHOUSE_USER=cliproxy_admin -e CLICKHOUSE_PASSWORD cliproxy-clickhouse clickhouse-client
```

**Search my history.** Each request resends the whole conversation, so `client_request` repeats every message hundreds of times: a week of it is about 275 GiB uncompressed, and a substring scan over it runs for minutes. Search `request_tail` and `response` instead. `request_tail` holds the messages a request added after the assistant's last turn (the typed prompt, tool results; see `schema/002_request_tail.sql`), and `response` holds the assistant's turn, so the two hold each message of a conversation once. A request's top-level system prompt, instructions, and tool definitions are in neither, nor is history a session starts with (a forked subagent's copy of its parent's conversation).

```sql
-- Sessions that mention a term, newest first
SELECT session_id, machine, min(started_at) AS first_seen, max(started_at) AS last_seen, count() AS requests
FROM cliproxy_log.requests
WHERE (request_tail ILIKE '%zebrafinch%' OR response ILIKE '%zebrafinch%')
  AND started_at >= now() - INTERVAL 30 DAY
GROUP BY session_id, machine
ORDER BY last_seen DESC
LIMIT 20;

-- Where the term was last said in each session, with 300 characters of context
SELECT started_at, machine, session_id,
       substring(request_tail, greatest(1, positionCaseInsensitive(request_tail, 'zebrafinch') - 150), 300) AS context
FROM cliproxy_log.requests
WHERE request_tail ILIKE '%zebrafinch%'
ORDER BY started_at DESC
LIMIT 1 BY session_id
LIMIT 20;
```

### Backfill request_tail

`install.sh` adds `request_tail` (`schema/002_request_tail.sql`) as a `MATERIALIZED` column: rows inserted from then on get it on insert, but rows already in the table compute it from `client_request` on every read, which is as slow as the scan it replaces. Write it into the existing rows once, as the admin (root on ThinkPad):

```sh
CLICKHOUSE_PASSWORD="$(cat ~/.config/cliproxy-log/admin-password)" sudo --preserve-env=CLICKHOUSE_PASSWORD \
  docker exec -i -e CLICKHOUSE_USER=cliproxy_admin -e CLICKHOUSE_PASSWORD cliproxy-clickhouse \
  clickhouse-client -q "ALTER TABLE cliproxy_log.requests MATERIALIZE COLUMN request_tail"   # returns at once; the mutation runs in the background
```

The mutation runs in the background, part by part, and the proxy keeps logging meanwhile. Load can rise as soon as `install.sh` adds the column, before the backfill starts: every routine merge of older parts computes the column for their rows. Follow it with `SELECT command, parts_to_do, is_done, latest_fail_reason FROM system.mutations WHERE table = 'requests' AND NOT is_done`, as the admin. A backfill of a 1-in-20 sample of the first week's rows (7,394 rows, 14.3 GiB of `client_request`) took 82 s and 111 CPU-seconds on ThinkPad, using under 2 cores; the whole table (about 148,000 rows on 2026-11-16) takes about 37 CPU-minutes, so expect 25 to 45 minutes, and more as the table grows. Check within the first two minutes that `parts_to_do` falls and `system.part_log` shows `MutatePart` rows with `error = 0`; a stuck mutation retries its failing part forever and blocks merges of those parts. Until it finishes, a search that reaches a part not yet rewritten parses `client_request` again and can time out. Never rerun it as part of `install.sh`: each run rewrites the column in every part.

**Tokens per machine, model, and account per day:**

```sql
SELECT toDate(started_at) AS day, machine, served_model, account,
       count() AS requests,
       sum(input_tokens) AS input, sum(output_tokens) AS output,
       sum(cache_read_tokens) AS cache_read, sum(cache_write_tokens) AS cache_write
FROM cliproxy_log.requests
WHERE started_at >= now() - INTERVAL 30 DAY
GROUP BY day, machine, served_model, account
ORDER BY day DESC, input DESC;
```

## Storage design and measurements

`schema/001_requests.sql` explains each choice in comments. The ones that matter:

- **Sort by session, not time.** `ORDER BY (machine, session_id, started_at)`. Consecutive requests of a session are ~95% identical, so placing them next to each other lets ZSTD store each one as a reference to the previous. Concurrent sessions interleave in time order and lose this.
- **Large compression blocks on the body columns** (16-32 MiB instead of 64 KiB-1 MiB), so a block holds dozens of turns rather than one or two.
- **ZSTD long-range window (16 MiB) on request bodies**, so a multi-megabyte long-context request still finds the previous turn.
- **`request_tail` for search** (`schema/002_request_tail.sql`): each request's new messages, so a search reads each message once instead of once per later turn. On 2026-11-16, after one week (138,000 rows), it held 817 MiB uncompressed and 185 MiB on disk, against 275 GiB and 86 GiB for `client_request`; FleetOK's search over all of it (`request_tail` and `response`) took about 0.6 s, where the same search over `client_request` ran past 60 s. Computing it costs 7 to 30 ms of CPU per row on insert, mostly parsing the request JSON.
- **Text indexes** (`text`, GA in 26.x) on `client_request` and `response`. They speed up case-sensitive `LIKE` and `hasToken` for rare tokens (the synthetic benchmark below); `ILIKE` does not use them, and on real traffic common tokens appear in every part, so history search reads `request_tail` instead. `upstream_request` is not indexed: it holds the same client-format payload after credential selection and model rewriting (translation to the upstream protocol happens later, where plugins cannot see it), so it repeats the text of `client_request`.
- Monthly partitions, a `minmax` index on `started_at` for time ranges, a bloom filter on `request_id`, and no TTL: data is kept forever for now.

Measured with `bench/bench.sh 192` (192 synthetic conversations of 40 turns, 7,680 requests, average request 212 KiB, largest 421 KiB) on the pinned image, against a naive table with the same columns ordered by time with default compression:

| Column | Uncompressed | This schema | Ratio | Naive (time order, LZ4) | Ratio |
| --- | --- | --- | --- | --- | --- |
| `client_request` | 1.53 GiB | 32.5 MiB | 48x | 899 MiB | 1.7x |
| `upstream_request` | 1.53 GiB | 32.5 MiB | 48x | 899 MiB | 1.7x |
| `response` (raw SSE) | 534 MiB | 14.6 MiB | 37x | 26.2 MiB | 20x |
| Whole table incl. indexes | 3.59 GiB | 86.7 MiB | 42x | 1.78 GiB | 2x |

The text indexes take 6.8 MiB, under 0.2% of the raw data. Other variants tried on the same data (bodies only): time order with ZSTD(3) reaches 3.3x; session order with default 1 MiB blocks 9.4x, with 8 MiB blocks 30x, with 32 MiB blocks 48x; ZSTD(9) instead of ZSTD(3) saves about 13% at 8 MiB blocks but costs much more CPU on every merge.

Substring search over the same 7,680 requests (best of 3, warm cache, query condition cache off):

| Query | With text index | Same table, indexes disabled | Naive table |
| --- | --- | --- | --- |
| `client_request LIKE '%xylophonequartz%'` | 4 ms, reads 57 rows | 252 ms, scans 1.53 GiB | 466 ms |
| `hasToken(client_request, 'xylophonequartz42')` | 3 ms | 328 ms | 466 ms |
| `ILIKE` grouped by session | 8 ms | 228 ms | 380 ms |
| `LIKE '%needle xylophonequartz42 is%'` (phrase) | 15 ms | 216 ms | 383 ms |

A full scan runs at about 6 GB/s of uncompressed text, so without the index a search over a year of history would take minutes; with it, lookups stay in milliseconds because only matching granules are read.

### Disk expectations

At about 8,000 requests a day, with requests of 200-600 KiB and the `upstream_request` copy of each, the raw volume is roughly 3-5 GB a day, 1.2-1.8 TB a year. At the measured 42x that is 30-45 GB a year on disk. Real traffic compresses less well than the synthetic conversations (context compaction, edited system reminders, and sessions without a session ID all break the shared prefix), so plan for 20-40x: **about 30-90 GB a year**, against 779 GB free on ThinkPad. Check the real ratio after a few weeks:

```sql
SELECT formatReadableSize(sum(data_uncompressed_bytes)) AS raw, formatReadableSize(sum(bytes_on_disk)) AS on_disk,
       round(sum(data_uncompressed_bytes) / sum(bytes_on_disk), 1) AS ratio
FROM system.parts WHERE database = 'cliproxy_log' AND active;
```

Run it as `cliproxy_admin` (see Query for the command).

## Backup

`cliproxy-log-backup.timer` runs `~/.local/bin/cliproxy-log-backup` nightly at about 03:30, catching up after the laptop was off. Each run:

1. `BACKUP DATABASE cliproxy_log TO Disk('backups', 'cliproxy_log')`, sent over HTTP to `127.0.0.1:8123` as `cliproxy_backup` with the password from `backup-password` (in a header read from a file descriptor, never on a command line or in the environment): a consistent copy in `~/.local/share/cliproxy-log/backups/cliproxy_log`, taken while the plugin keeps writing. The run needs no Docker access and no root.
2. `restic backup` of that directory into `sftp:storageserver:/home/cliproxy-log-backups/thinkpad`, which is `~/cliproxy-log-backups/thinkpad` in the NAS login's home. This uses the same SSH/SFTP route as the fleet credential backup but its own repository and `restic-password`, so the two never share keys or retention. Unchanged data parts deduplicate, but background merges rewrite and recompress parts within the current month, so a night can upload several times what was written that day.
3. `restic forget --prune` keeping 7 daily, 4 weekly, and 6 monthly snapshots, then `restic check` on a 5% sample.
4. Deletes the local copy (also when a step fails).

`cliproxy-log-backup --check` only checks that ClickHouse answers and `cliproxy_backup` logs in with BACKUP, and says which failed (`install.sh` runs it from the timer's systemd user manager; `--check-docker` is an older name for it). The staging copy needs free space equal to the database size during the run. Logs: `journalctl --user -u cliproxy-log-backup`. Run one now: `systemctl --user start cliproxy-log-backup` (or `cliproxy-log-backup` in a terminal).

Settings can be overridden with environment variables documented at the top of `backup.sh`, for example `CLIPROXY_LOG_RESTIC_REPOSITORY` for a different target.

## Restore

```sh
export RESTIC_PASSWORD_FILE=~/.config/cliproxy-log/restic-password
repo=sftp:storageserver:/home/cliproxy-log-backups/thinkpad
restic -r "$repo" snapshots

# Restore into the directory ClickHouse sees as its backups disk
restic -r "$repo" restore latest --target ~/.local/share/cliproxy-log/backups/restore
find ~/.local/share/cliproxy-log/backups/restore -name .backup   # note the directory holding .backup
```

Then, as admin (root on ThinkPad; the command is under Query), with `<dir>` being that directory relative to `backups/restore/` (restic recreates the full original path, so it looks like `home/loadinglucian/.local/share/cliproxy-log/backups/cliproxy_log`):

```sql
-- Side by side, to inspect or copy rows back
RESTORE DATABASE cliproxy_log AS cliproxy_log_restored FROM Disk('backups', 'restore/<dir>');

-- Or replace the live database (the plugin's inserts fail until it is back)
DROP DATABASE cliproxy_log SYNC;
RESTORE DATABASE cliproxy_log FROM Disk('backups', 'restore/<dir>');
```

Finally `rm -rf ~/.local/share/cliproxy-log/backups/restore`. CI performs this round trip on every change (`test/smoke.sh --with-backup`).

On a new computer, put the restic password from Proton Pass into `~/.config/cliproxy-log/restic-password` (0600, inside a 0700 directory) **before** running `install.sh`, which otherwise generates a new one. The other passwords can be new; reconfigure the plugin with the new `writer-password`.

## Uninstall

```sh
systemctl --user disable --now cliproxy-log-backup.timer
rm -rf ~/.config/systemd/user/cliproxy-log-backup.{service,timer,service.d} ~/.local/bin/cliproxy-log-backup
systemctl --user daemon-reload
sudo docker compose -f deploy/clickhouse/docker-compose.yml down
```

That keeps the data and passwords. To delete them too (irreversible): `rm -rf ~/.local/share/cliproxy-log ~/.config/cliproxy-log`, and on the NAS `ssh storageserver 'rm -rf ~/cliproxy-log-backups'`. Disable the plugin first so it stops trying to write.

## Benchmark

```sh
deploy/clickhouse/bench/bench.sh [sessions]   # default 48 sessions, about 1 GB; 192 used above
```

It runs in a throwaway container and touches nothing else.
