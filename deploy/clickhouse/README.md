# cliproxy-log ClickHouse

A local ClickHouse database that stores every model request CLIProxyAPI handles, with full request and response bodies, for searching past conversations and analysing usage per machine, model, and account. The cliproxy-log plugin (`plugins/cliproxy-log`) writes one row per request into `cliproxy_log.requests` over HTTP on `127.0.0.1:8123`. Nothing outside the computer can reach it.

| Path | What it is |
| --- | --- |
| `docker-compose.yml` | The server: `clickhouse/clickhouse-server:26.8.34.13` (26.8 LTS), loopback-only, 16 GiB memory cap |
| `config/cliproxy.xml` | Server overrides: console logging, backups disk, system-log retention |
| `config/users.xml.template` | The three users; `install.sh` fills in password hashes |
| `schema/001_requests.sql` | Database and table DDL, safe to rerun |
| `install.sh` | Installs or updates everything on this computer |
| `backup.sh`, `systemd/` | Nightly backup to the NAS |
| `bench/` | Storage and search benchmark on synthetic traffic |
| `test/smoke.sh` | End-to-end check used by CI (`.github/workflows/fork-clickhouse.yml`) |

## Install

Prerequisites on ThinkPad (one time, needs sudo):

```sh
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"     # then reboot, so the backup timer gets the group too
sudo pacman -S --needed restic
```

Then, from a checkout of this repository:

```sh
deploy/clickhouse/install.sh
```

It is safe to rerun after pulling changes. It:

1. Creates `~/.config/cliproxy-log` (0700) and `~/.local/share/cliproxy-log/{clickhouse,backups}`, marking the data directories no-copy-on-write (`chattr +C`) on btrfs while they are still empty. ClickHouse compresses its own files, so btrfs compression and copy-on-write only cost CPU and fragmentation.
2. Generates four passwords once (0600 files, never printed): `admin-password`, `writer-password`, `reader-password`, `restic-password`.
3. Renders `~/.config/cliproxy-log/users.d/cliproxy-users.xml` with SHA-256 hashes of those passwords.
4. Starts the container, applies every `schema/*.sql`, and checks each user's permissions over HTTP.
5. Installs `~/.local/bin/cliproxy-log-backup` and enables the `cliproxy-log-backup.timer` user unit with the directories used here (skip with `--no-timer`).

It never touches CLIProxyAPI. **Save `~/.config/cliproxy-log/restic-password` in Proton Pass** (for example as "cliproxy-log restic backup"): without it the NAS backups cannot be decrypted.

The container runs as your user, so all files stay yours. To run `docker compose` by hand, export what `install.sh` exports:

```sh
export CLIPROXY_LOG_UID=$(id -u) CLIPROXY_LOG_GID=$(id -g)
docker compose -f deploy/clickhouse/docker-compose.yml ps
```

To upgrade ClickHouse, change the image tag and digest in `docker-compose.yml` (stay on an LTS line, `.3` or `.8`) and rerun `install.sh`.

## Users

| User | Password file | Can do | Reachable from |
| --- | --- | --- | --- |
| `cliproxy_writer` | `writer-password` | `INSERT`, `SELECT` on `cliproxy_log.*` | `127.0.0.1:8123` |
| `cliproxy_reader` | `reader-password` | `SELECT` on `cliproxy_log.*`; queries stop after 300 s or 8 GiB and the limits cannot be raised | `127.0.0.1:8123` |
| `cliproxy_admin` | `admin-password` | everything (schema changes, backups) | inside the container only (`docker exec`) |

There is no `default` user and no passwordless access. Password files hold 64 hex characters without a trailing newline. Inserts by the writer are batched server-side (`async_insert`) and return once written.

## Query

**Play UI**: open <http://127.0.0.1:8123/play> on ThinkPad, enter `cliproxy_reader` and the contents of `reader-password`. From another computer, tunnel first: `ssh -L 8123:127.0.0.1:8123 thinkpad`.

**clickhouse-client**: the password goes through the environment, not the command line:

```sh
CLICKHOUSE_PASSWORD="$(cat ~/.config/cliproxy-log/reader-password)" \
  docker exec -it -e CLICKHOUSE_USER=cliproxy_reader -e CLICKHOUSE_PASSWORD \
  cliproxy-clickhouse clickhouse-client
```

**Search my history.** The newest request of a session contains every earlier message of it, user and assistant, so searching `client_request` covers whole conversations. `LIKE`/`ILIKE` patterns with at least 4 letters or digits, and `hasToken`, use the text index and answer in milliseconds:

```sql
-- Sessions that mention a term, newest first
SELECT session_id, machine, min(started_at) AS first_seen, max(started_at) AS last_seen, count() AS requests
FROM cliproxy_log.requests
WHERE client_request ILIKE '%zebrafinch%'
GROUP BY session_id, machine
ORDER BY last_seen DESC
LIMIT 20;

-- Where the term first appeared in each session, with 300 characters of context
SELECT started_at, machine, session_id,
       substring(client_request, greatest(1, positionCaseInsensitive(client_request, 'zebrafinch') - 150), 300) AS context
FROM cliproxy_log.requests
WHERE client_request ILIKE '%zebrafinch%'
ORDER BY started_at DESC
LIMIT 1 BY session_id
LIMIT 20;
```

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
- **Text indexes** (`text`, GA in 26.x) on `client_request` and `response`. `upstream_request` is a translated copy of `client_request` and is not indexed.
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

At about 8,000 requests a day, with requests of 200-600 KiB and the upstream copy of each, the raw volume is roughly 3-5 GB a day, 1.2-1.8 TB a year. At the measured 42x that is 30-45 GB a year on disk. Real traffic compresses less well than the synthetic conversations (context compaction, edited system reminders, and sessions without a session ID all break the shared prefix), so plan for 20-40x: **about 30-90 GB a year**, against 779 GB free on ThinkPad. Check the real ratio after a few weeks:

```sql
SELECT formatReadableSize(sum(data_uncompressed_bytes)) AS raw, formatReadableSize(sum(bytes_on_disk)) AS on_disk,
       round(sum(data_uncompressed_bytes) / sum(bytes_on_disk), 1) AS ratio
FROM system.parts WHERE database = 'cliproxy_log' AND active;
```

Run it as `cliproxy_admin` (see Restore for the command).

## Backup

`cliproxy-log-backup.timer` runs `~/.local/bin/cliproxy-log-backup` nightly at about 03:30, catching up after the laptop was off. Each run:

1. `BACKUP DATABASE cliproxy_log TO Disk('backups', 'cliproxy_log')`: a consistent copy in `~/.local/share/cliproxy-log/backups/cliproxy_log`, taken while the plugin keeps writing.
2. `restic backup` of that directory into `sftp:storageserver:/home/cliproxy-log-backups/thinkpad`, which is `~/cliproxy-log-backups/thinkpad` in the NAS login's home. This uses the same SSH/SFTP route as the fleet credential backup but its own repository and `restic-password`, so the two never share keys or retention. Unchanged data parts deduplicate, so a night uploads roughly what was written that day.
3. `restic forget --prune` keeping 7 daily, 4 weekly, and 6 monthly snapshots, then `restic check` on a 5% sample.
4. Deletes the local copy (also when a step fails).

The staging copy needs free space equal to the database size during the run. Logs: `journalctl --user -u cliproxy-log-backup`. Run one now: `systemctl --user start cliproxy-log-backup` (or `cliproxy-log-backup` in a terminal).

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

Then, as admin (`CLICKHOUSE_PASSWORD="$(cat ~/.config/cliproxy-log/admin-password)" docker exec -it -e CLICKHOUSE_USER=cliproxy_admin -e CLICKHOUSE_PASSWORD cliproxy-clickhouse clickhouse-client`), with `<dir>` being that directory relative to `backups/`:

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
docker compose -f deploy/clickhouse/docker-compose.yml down
```

That keeps the data and passwords. To delete them too (irreversible): `rm -rf ~/.local/share/cliproxy-log ~/.config/cliproxy-log`, and on the NAS `ssh storageserver 'rm -rf ~/cliproxy-log-backups'`. Disable the plugin first so it stops trying to write.

## Benchmark

```sh
deploy/clickhouse/bench/bench.sh [sessions]   # default 48 sessions, about 1 GB; 192 used above
```

It runs in a throwaway container and touches nothing else.
