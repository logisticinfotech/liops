# liops monitoring: design and build plan

Status: **agreed** (November 2026), revised after a real investigation (§1.1); not started.
[PLAN.md](PLAN.md) stays the source of truth for everything this document doesn't change; read
it first.

## 1. Requirement

A small, per-server tool that answers the questions the team actually asks about an app server:

- **Which requests are slow, and why?** Across nginx, Apache and Caddy, in front of PHP-FPM,
  Go (Goravel), and sometimes Python or Node apps.
- **Is the database the problem?** Which queries take the most time on MySQL and PostgreSQL.
- **Tune it, fix the app, or get a bigger VM?** A plain verdict, with the evidence for it.
- **CPU, memory and disk** for the last 7 days, with trends long enough to plan capacity.
- **Logs**: read the web server, PHP-FPM, app and system logs in one place.

### 1.1 Why the database comes first: a real case

Two multi-tenant Laravel apps on near-identical VMs (Ubuntu 26.04, nginx, PHP-FPM with
OPcache, a local database). App A (PostgreSQL 18, little data) answered in about 60 ms; app B
(MySQL 8.4, about 30 tenant databases, about 1 GB, devices pushing data all day, several queue
workers) in about 440 ms, as it had on its previous hosting. Server or code?

1. Browser timings: connect and TLS were the same; the **time was on the server**.
2. The app's config (debug off, caches built) ruled out a misconfigured framework. A debug log
   line written per record in a loop (about 40 lines a second) was noise, not the cause.
3. A cheap page timed from the server itself (login: about 30 ms) ruled out the framework
   start, tenant lookup, database connection and session: **the cost was in certain pages**.
4. One minute of MySQL's slow log with `long_query_time=0`, while loading a slow page: about
   10,600 queries, 33 s in total; summarised by total time and by query shape.
5. The queries over 1 s: a global search, `LOWER(col) LIKE '%term%'` on five columns, run twice
   per keystroke, examining **1.65 million rows to return 1**, 2.5–2.9 s each; and a dashboard
   filter `WHERE DATE(col) = …` (the function stops index use; no index on the columns
   anyway), 1.4–2.2 s, three times per page.
6. `SHOW INDEX` and row counts confirmed missing indexes. The fix was in the code: two
   indexes, a filter without the function, and limits on the search.

App A had the same scans; its tables were just still small. And each scan held a CPU core for
1–3 s, slowing unrelated requests too.

**Each manual step is a signal liops can collect all the time**, cheaply, from the databases'
own statistics (§4.4). That makes **query efficiency the headline** of the monitoring: in the
UI, in the daily email, and first in the build order (§8).

Not in scope (decided October 2026):

- **Uptime and SSL checks.** An outside tool already does them, and a server can't report its
  own death.
- **A central dashboard.** One UI per server, like the backups (PLAN.md §2). A central view can
  be added later by having each server push a summary; nothing here prevents it.
- **Full-text log search and log retention.** liops reads the logs that are already on the
  server; it doesn't index or copy them.
- **Tracing inside apps** (OpenTelemetry). The proxy's timings and the database's query stats
  answer "which request, which query" for most cases; tracing is a later option (§10).

## 2. Decisions

| Topic | Decision |
|---|---|
| Where | **The same liops repo, binary and UI**: a "Monitoring" section next to Backups. One install, one tunnel, one place to look. |
| Process | **A separate service, `liops-monitor`**, running `liops monitor` as an **unprivileged user** (`liops-monitor`), not root. The backup service holds the storage keys and runs as root; monitoring reads logs and parses untrusted text all day. A bug or a crash in monitoring must not touch backups or credentials. |
| Data path | The monitor process collects and stores; the existing UI (root) asks it for data over a **Unix socket** (`/run/liops-monitor/monitor.sock`). The monitor never gets the backup service's config or secrets. |
| Measuring requests | **At the front web server**, from its access log: one place that sees every request, whatever language is behind it. No agent inside apps. |
| Asking first | Changes to web server, PHP-FPM or database settings (a log format, a status page, `pg_stat_statements`) are **listed and applied only after a person approves**, like the PostgreSQL restart and MySQL GTIDs. Config is checked (`nginx -t`, `apachectl configtest`, `php-fpm -t`) before a **reload**; nothing is restarted without asking. |
| Database access | Read-only accounts with no password, and **no access to the app's data**: PostgreSQL role `liops_monitor` with `pg_monitor`, by peer authentication; MySQL `'liops_monitor'@'localhost'` `IDENTIFIED WITH auth_socket AS 'liops-monitor'`, with `PROCESS`, `SELECT` on `performance_schema` and `sys`, and `REFERENCES` on `*.*` (lets it see tables and indexes in `information_schema`, not read rows). Nothing to store. |
| Query statistics | **From the databases' own summaries, never from a slow log**: `performance_schema` digests and `pg_stat_statements` are always on, cheap and already normalised. Snapshots **every 5 minutes**, stored as differences. A slow log with `long_query_time=0` is fine for a one-minute test, too heavy to leave on. |
| Real query values | MySQL keeps one **sample statement** per query shape, with real values (search terms, names). It is **read live when a person opens that query in the UI, never stored, logged or emailed**. Stored and emailed: only the normalised shape (`?` / `$1`). |
| History | Raw samples (every 15 s) in memory for the last hour; **1-minute rollups for 7 days** and **hourly rollups for 90 days** on disk. 7 days to diagnose, 90 days for trends ("disk full in 40 days", "memory has grown 30% since August"). |
| Storage | bbolt, as for the backup state, in its own file (`/var/lib/liops-monitor/metrics.db`): one record per minute (or hour) holding every series, compactly encoded. No time-series database to install. |
| Charts | **Drawn on the server as SVG**: no JavaScript chart library, and nothing the strict CSP has to allow. |
| Footprint | Budget: **under 50 MB of memory and 1% of a CPU** on a busy server. Logs are read incrementally (offsets, rotation by inode), never re-read. |
| Alerts | Few, and through the existing email: a "server health" section in the daily summary, plus immediate emails for disk filling soon, memory exhaustion (OOM kills), and a sustained burst of 5xx errors. No pager-style alerting. |
| Dependencies | **None new.** /proc, statfs, a small FastCGI client (for PHP-FPM's status page) and the existing `mysql`/`psql` client programs cover everything. |

## 3. Architecture

```
 One app server
 ┌───────────────────────────────────────────────────────────────────────────┐
 │  nginx / Apache / Caddy ──access logs──┐                                   │
 │  PHP-FPM ──status page, slow log───────┤                                   │
 │  Goravel, Python, Node (systemd units)─┤ cgroup CPU/memory                │
 │  MySQL / PostgreSQL ──query stats──────┤                                   │
 │  /proc, statfs, journald ──────────────┤                                   │
 │                                        ▼                                   │
 │                 liops-monitor (user liops-monitor, groups adm, …)          │
 │                 collect every 15 s · parse logs · roll up · verdict rules  │
 │                 /var/lib/liops-monitor/metrics.db                          │
 │                        │ Unix socket (read API, JSON)                      │
 │                        ▼                                                   │
 │                 liops serve (root): UI on 127.0.0.1:8899, emails           │
 └───────────────────────────────────────────────────────────────────────────┘
```

- `liops monitor` is a subcommand of the same binary. `install-system` installs the second
  systemd unit and creates the user; the monitor is **off until turned on** in Settings
  (`monitor.enabled`), so a server that only wants backups is unchanged.
- If the monitor isn't running, the UI says so on the Monitoring pages; backups don't care.

## 4. What it collects

### 4.1 The machine

From `/proc` and `statfs`, every 15 s:

- **CPU**: user, system, **iowait** and **steal** (a busy neighbour on the same host) separately;
  load average.
- **Memory**: used, cache, available, **swap in/out**; **OOM kills** (from the kernel log).
- **Disk**: use per filesystem, and growth per day (from the hourly rollups); I/O per device:
  throughput, utilisation, average wait.
- **Network**: throughput and errors per interface.
- **Load average against the number of CPUs** (a load of 8 means something else on 2 CPUs
  than on 16).
- **Per service**: CPU and memory of each systemd service (from its cgroup): nginx, php8.x-fpm,
  each app's unit, mysql/postgresql. This is how Go, Python and Node apps are measured
  without touching them.
- **Top processes** by CPU and memory, grouped by command (e.g. `php artisan queue:work` ×7):
  queue workers often all run under one supervisor service, so per service isn't enough.

### 4.2 Web requests

The front web server's access log, tailed. Each request gives method, **route**, status,
**time taken** (and, behind a proxy, the upstream's time), bytes.

- **Routes, not URLs**: `/orders/12345?page=2` becomes `/orders/{n}`, so similar requests
  add up. Numbers, UUIDs and long hex strings become placeholders; query strings are dropped.
  A per-server list of route patterns can override it (e.g. `/t/{tenant}/…`).
- Per route, per minute: count, **p50 / p95 / p99** time, 4xx and 5xx counts. Per server: the
  same, plus the **slowest requests** of the last hour (kept individually, with time, status
  and upstream).
- **Formats**:
  - **nginx**: needs `$request_time` and `$upstream_response_time` in the log format.
    Setup proposes a `log_format liops …` in `conf.d` and the one-line `access_log` change per
    site, shows them, and applies them after approval (`nginx -t`, then reload).
  - **Apache**: not used on these servers (§9); its parser (`%D`) is left for later.
  - **Caddy**: its JSON access logs already include the duration; the site needs `log`.
- **Timeouts and slow upstreams** show up as both: a 502/504 count and the upstream time.

### 4.3 PHP-FPM

- **Status page** of each pool (`pm.status_path`, read over FastCGI on the pool's socket):
  active and idle workers, **listen queue**, and **"max children reached"**: the most common
  reason PHP sites are slow while the CPU is idle.
- **Slow log** (`request_slowlog_timeout`): the PHP stack trace of each request slower than
  the threshold, shown next to the slow requests.
- Setup proposes the two pool settings, with the same approval and `php-fpm -t` check.

### 4.4 Databases: query efficiency first

Collected per **database (schema)**, because in a multi-tenant app the same query shape is slow
in the big tenants' databases and fine in the small ones.

**Per query shape, every 5 minutes** (snapshot, minus the previous one; a lower counter means
the statistics were reset, and the snapshot starts again):

| | MySQL 8.4: `performance_schema.events_statements_summary_by_digest` | PostgreSQL: `pg_stat_statements` (+ `pg_database`) |
|---|---|---|
| Database | `SCHEMA_NAME` | `dbid` |
| Shape | `DIGEST_TEXT` (literals as `?`) | `query` (literals as `$1`) |
| Calls, total and max time | `COUNT_STAR`, `SUM_TIMER_WAIT`, `MAX_TIMER_WAIT` | `calls`, `total_exec_time`, `max_exec_time` |
| **Rows examined ÷ rows sent** | `SUM_ROWS_EXAMINED / SUM_ROWS_SENT` | approximate: `(shared_blks_hit + shared_blks_read) / rows` |
| No usable index | `SUM_NO_INDEX_USED`, `SUM_NO_GOOD_INDEX_USED` | per table, below |
| Disk temp tables, sorts | `SUM_CREATED_TMP_DISK_TABLES`, `SUM_SORT_ROWS` | `temp_blks_written` |

**Per table, every hour**, for every database: row count and size, and for PostgreSQL
`seq_scan` vs `idx_scan` and `seq_tup_read` (`pg_stat_user_tables`, read per database). That
answers "which tables are growing into a problem" and, on PostgreSQL, "which tables are read
whole".

**Right now**, every 15 s: running statements older than 1 s (MySQL
`performance_schema.events_statements_current` / process list; PostgreSQL `pg_stat_activity`,
with lock waits and idle-in-transaction sessions), connections, and the buffer cache hit ratio.

**The headline: "These queries scan whole tables"**, ranked by total time, with:

- the database, calls, total time, time per call, and **rows examined per row sent** (1.65
  million to 1 in §1.1);
- **pattern flags** on the shape: a column inside a function in a filter (`DATE(col) =`,
  `LOWER(col) LIKE`, `YEAR(col)`), `LIKE` with a leading wildcard (on MySQL visible only in the
  sample statement: the shape shows `LIKE ?`), `OR` across columns, `SELECT *` with a large
  rows-sent count;
- **the existing indexes of the tables it touches**, side by side with the shape;
- for MySQL, **one real sample statement**, read live on request (§2).

Also: `SHOW GLOBAL STATUS` (MySQL: threads, buffer pool reads vs requests, temporary tables on
disk); PostgreSQL checkpoints and cache hit ratio.

**Setup**: MySQL's digests are on by default. PostgreSQL needs `pg_stat_statements` in
`shared_preload_libraries` and one restart, asked first (agreed, §9). Without it, the database
page still shows the per-table scans, activity and locks.

### 4.5 Logs

A viewer, not a store. Sources: the systemd journal per service (`journalctl -u …`), and log
files: the web server's error logs, PHP-FPM's log and slow log, and app log files listed in the
config (e.g. Laravel/Goravel's `storage/logs/*.log`).

- Show the last lines, follow live, filter by text, level (when the format has one) and time.
- Hard limits on how much is read per request, so a 20 GB log can't slow the server.
- **Growth rate per log file** (lines and bytes per minute), on the Resources page: a debug log
  line written in a loop (§1.1) costs disk and I/O, and is easy to miss.
- Logs can contain secrets and personal data. They stay behind the same SSH tunnel as
  everything else, and the monitor's user only gets read access to the logs it's given.

## 5. The verdict: tune it, fix the app, or a bigger VM?

The point of the tool. Plain rules over the last 24 hours and 7 days, each shown with the
numbers and the charts behind it; liops never claims more than the data shows:

| Evidence | Says |
|---|---|
| High **steal** | The host is overcommitted: move the VM, or a different plan; tuning won't help |
| High **iowait**, disk utilisation near 100% | Disk-bound: faster disk, or more memory for the database's cache |
| **Swap** in use, OOM kills, little memory available | Memory-bound: more RAM, or fewer PHP workers / smaller DB buffers |
| PHP **max children reached** / listen queue > 0 while CPU has room | Raise `pm.max_children` (with the memory per worker it would cost) |
| A few query shapes take most of the database's time; rows examined ≫ rows sent, or no index used | **Fix the app first**: those queries need indexes or rewriting (listed, with the tables' indexes and pattern flags) |
| Long statements (> 1 s) while CPU is high and unrelated routes slow down | The scans are holding CPU cores: fixing them speeds up the rest too |
| A log file growing by thousands of lines a minute | Logging too much (e.g. a debug level left on): costs disk and I/O |
| Database cache hit ratio low while memory is free | Give the database more memory (`innodb_buffer_pool_size`, `shared_buffers`) |
| CPU busy (> 80%) for long stretches, spread across services, no single cause | The workload has outgrown the VM: more CPUs |
| Slow routes with low database time and low CPU | Time is spent waiting elsewhere: external APIs, locks, sleeps; see the slow log |
| Disk growth trend | "Full in about N days" |

## 6. UI

A "Monitoring" section in the same UI, through the same tunnel:

- **Overview**: the verdict, the problems of the last 24 hours, and small charts of CPU,
  memory, disk, requests and p95 time.
- **Requests**: slowest routes (p95, p99, count, total time), status codes, the slowest
  individual requests, upstream vs total time.
- **PHP-FPM**: per pool, workers and queue over time, slow log entries.
- **Database** (first in the menu): **"These queries scan whole tables"** (§4.4), then top
  queries by total time, per database; a query's detail page with its shape, pattern flags,
  the tables' indexes and, for MySQL, the live sample; biggest and fastest-growing tables;
  running statements, connections, cache hit ratio.
- **Resources**: CPU (with iowait and steal), memory, swap, disk, I/O, network, per-service
  CPU and memory.
- **Logs**: the viewer.

Time ranges: last hour (15 s), 24 hours and 7 days (1 minute), 90 days (1 hour). Query
statistics: 5-minute snapshots for 7 days, daily totals for 90 days (top 50 shapes per
database per period, to bound storage).

**Daily summary email**: a "Server health" section led by the query headline ("3 queries scan
whole tables: 41 min of database time yesterday", with shape, database and ratio, never
values), then the verdict's other findings.

## 7. Config

```yaml
monitor:
  enabled: false            # off until turned on in Settings
  slow_request: 1s          # requests slower than this are "slow"
  access_logs: []           # empty: found from nginx/Apache/Caddy's config
  php_fpm_pools: []         # empty: found from /etc/php/*/fpm/pool.d
  services: []              # systemd units to watch; empty: every running service
  log_files: []             # extra log files for the viewer, globs allowed
  routes: []                # route patterns that override the automatic ones
```

What's detected automatically is shown in Settings, with the source it came from.

## 8. Build phases

Reordered after §1.1: the database work comes right after the skeleton, because it's what found
the problem.

| Phase | Deliverable | Done when | Estimate |
|---|---|---|---|
| **MO0. Skeleton + machine** | `liops monitor`, the `liops-monitor` user and unit, the Unix-socket API, the rollup store, machine, per-service and top-process metrics (§4.1), the Resources page, SVG charts | A VM shows CPU/memory/disk history; the monitor stays under budget; backups unaffected with it stopped or crashing | 1 week |
| **MO1. Query efficiency** | MySQL digests and PostgreSQL `pg_stat_statements` (with the approved restart), per database; per-table stats; running statements; pattern flags; indexes side by side; the live MySQL sample; the Database page; the headline in the daily email | §1.1 reproduced on a VM: a seeded table of ~1M rows, a `LOWER(col) LIKE '%x%'` search and a `DATE(col) =` filter come out on top with their ratios, flags and missing indexes, in a multi-database setup | 1.5 weeks |
| **MO2. Requests and PHP-FPM** | nginx and Caddy log parsers, routes, percentiles, slowest requests, the approved log format change; PHP-FPM status and slow log (§4.2, §4.3); their pages | A load test against nginx + PHP-FPM shows the slow routes, and a starved pool shows as "max children reached" | 1.5 weeks |
| **MO3. Verdict, logs, alerts** | The verdict rules (§5), the log viewer and log growth (§4.5), the immediate alerts | On scripted scenarios (CPU hog, memory pressure, bad query, small `pm.max_children`, full-ish disk, chatty log) the verdict names the right cause | 1 week |

Testing follows the backups: unit tests with fakes for every parser and rule, and a
`make vm-test-monitor` that builds a LEMP VM (nginx, PHP-FPM, MySQL, and PostgreSQL for MO1),
drives load at it, and checks what the monitor reports.

## 9. Risks and open questions

Answered (November 2026):

- **Stacks**: web servers are **nginx and Caddy** (no Apache: its parser is left for later);
  PHP **8.2, 8.3, 8.4 and 8.5**, sometimes several on one server (one FPM service and pool
  directory each: `php8.x-fpm`, `/etc/php/8.x/fpm/pool.d`); databases **PostgreSQL and MySQL**.
- **Log format changes** on live sites are fine, with approval and a config check before reload.
- **The PostgreSQL restart** for `pg_stat_statements` is fine, once per server, scheduled by a
  person.

Still open:

- **`REFERENCES` on `*.*` for MySQL** (§2) is the least privilege that lets the monitor list
  every table's indexes without reading rows. Check on MySQL 8.4 in MO1 that it is enough
  for `information_schema.STATISTICS` and `TABLES`; if not, the index list comes from the root
  service instead.
- **Many tenant databases** (hundreds): the hourly per-table pass on PostgreSQL connects to
  each database; it's spread over the hour and capped, measured in MO1.

- **Some sites may have their own `access_log` lines**; setup lists each change per site.
- **Busy logs** (millions of lines a day): parsing is cheap in Go, but the budget in §2 is
  checked in the MO1 load test before it's called done.
- **Thresholds** (slow request, alerts) are guesses until they've run on your servers for a
  while; they're settings, and the first weeks should tune them.

## 10. Later

- **A central view**: each server pushes a small daily summary (or the verdict) to a bucket,
  and one page lists all servers.
- **App metrics** from apps that expose Prometheus `/metrics` (Go apps can, with one library).
- **Tracing** (OpenTelemetry) for requests whose time isn't explained by the database or PHP.
