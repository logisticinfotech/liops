# liops monitoring: design and build plan

Status: **proposed** (November 2026). Nothing here is built yet. [PLAN.md](PLAN.md) stays the
source of truth for everything this document doesn't change; read it first.

## 1. Requirement

A small, per-server tool that answers the questions the team actually asks about an app server:

- **Which requests are slow, and why?** Across nginx, Apache and Caddy, in front of PHP-FPM,
  Go (Goravel), and sometimes Python or Node apps.
- **Is the database the problem?** Which queries take the most time on MySQL and PostgreSQL.
- **Tune it, fix the app, or get a bigger VM?** A plain verdict, with the evidence for it.
- **CPU, memory and disk** for the last 7 days, with trends long enough to plan capacity.
- **Logs**: read the web server, PHP-FPM, app and system logs in one place.

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
| Database access | Read-only accounts with no password: PostgreSQL role `liops_monitor` with `pg_monitor`, by peer authentication; MySQL `'liops_monitor'@'localhost'` `IDENTIFIED WITH auth_socket AS 'liops-monitor'`. Nothing to store. |
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
- **Per service**: CPU and memory of each systemd service (from its cgroup): nginx, php8.x-fpm,
  each app's unit, mysql/postgresql. This is how Go, Python and Node apps are measured
  without touching them.

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
  - **Apache**: needs `%D` (microseconds). Same approach, with a `LogFormat` and `CustomLog`.
  - **Caddy**: its JSON access logs already include the duration; the site needs `log`.
- **Timeouts and slow upstreams** show up as both: a 502/504 count and the upstream time.

### 4.3 PHP-FPM

- **Status page** of each pool (`pm.status_path`, read over FastCGI on the pool's socket):
  active and idle workers, **listen queue**, and **"max children reached"**: the most common
  reason PHP sites are slow while the CPU is idle.
- **Slow log** (`request_slowlog_timeout`): the PHP stack trace of each request slower than
  the threshold, shown next to the slow requests.
- Setup proposes the two pool settings, with the same approval and `php-fpm -t` check.

### 4.4 Databases

- **MySQL 8.4**: `performance_schema.events_statements_summary_by_digest` (on by default):
  each query shape with its total time, calls, rows examined vs rows sent, and full scans,
  as deltas per minute. Also `SHOW GLOBAL STATUS` (threads, buffer pool hit ratio, temporary
  tables on disk) and long-running queries from the process list.
- **PostgreSQL**: `pg_stat_statements` (total time, calls, rows, shared buffer hits per query
  shape). It needs `shared_preload_libraries`, which needs **one restart**: setup asks first,
  as for WAL archiving. Also `pg_stat_activity` (connections, long-running and idle-in-
  transaction sessions, waits on locks), cache hit ratio, and checkpoints.
- **Top queries by total time** is the headline: if a handful of queries take most of the
  database's time, that's where the work is.
- Query text is normalised by the database (digests, `$1`), so no values from the app's data
  are stored.

### 4.5 Logs

A viewer, not a store. Sources: the systemd journal per service (`journalctl -u …`), and log
files: the web server's error logs, PHP-FPM's log and slow log, and app log files listed in the
config (e.g. Laravel/Goravel's `storage/logs/*.log`).

- Show the last lines, follow live, filter by text, level (when the format has one) and time.
- Hard limits on how much is read per request, so a 20 GB log can't slow the server.
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
| A few query shapes take most of the database's time; rows examined ≫ rows sent | **Fix the app first**: those queries need indexes or rewriting (listed) |
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
- **Database**: top queries by total time, connections, long-running queries, cache hit ratio.
- **Resources**: CPU (with iowait and steal), memory, swap, disk, I/O, network, per-service
  CPU and memory.
- **Logs**: the viewer.

Time ranges: last hour (15 s), 24 hours and 7 days (1 minute), 90 days (1 hour).

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

| Phase | Deliverable | Done when | Estimate |
|---|---|---|---|
| **MO0. Skeleton + machine** | `liops monitor`, the `liops-monitor` user and unit, the Unix-socket API, the rollup store, machine and per-service metrics (§4.1), the Resources page, SVG charts | A VM shows 7 days of CPU/memory/disk after a week; the monitor stays under budget; backups unaffected with it stopped or crashing | 1 week |
| **MO1. Requests** | Log tailing and parsers (nginx, Apache, Caddy), routes, percentiles, slowest requests; setup proposing the log format change with approval; the Requests page | A load test against a VM with nginx + PHP-FPM shows the slow routes and their times correctly | 1.5 weeks |
| **MO2. PHP-FPM and databases** | FastCGI status and slow log (§4.3); MySQL and PostgreSQL query stats (§4.4), including the approved `pg_stat_statements` restart; their pages | A deliberately slow query and a starved PHP pool show up as the top problem | 1.5 weeks |
| **MO3. Verdict, logs, alerts** | The verdict rules (§5), the log viewer (§4.5), the daily-summary section and the few immediate alerts | On scripted scenarios (CPU hog, memory pressure, bad query, small `pm.max_children`, full-ish disk) the verdict names the right cause | 1 week |

Testing follows the backups: unit tests with fakes for every parser and rule, and a
`make vm-test-monitor` that builds a LEMP VM (nginx, PHP-FPM, MySQL), drives load at it, and
checks what the monitor reports.

## 9. Risks and open questions

- **Which stacks run where?** For each server: the web server, PHP version(s), app services, and
  database. The detection in §4 is built for the common Ubuntu layouts; anything unusual goes
  in the config.
- **Changing web server log formats** on live sites: done only with approval, checked before
  reload. Some sites may have their own `access_log` lines; setup lists each change per site.
- **The PostgreSQL restart** for `pg_stat_statements`: one restart per server, scheduled by you.
  Without it, the database page still shows activity, locks and cache ratios, but not top
  queries.
- **Busy logs** (millions of lines a day): parsing is cheap in Go, but the budget in §2 is
  checked in the MO1 load test before it's called done.
- **Thresholds** (slow request, alerts) are guesses until they've run on your servers for a
  while; they're settings, and the first weeks should tune them.

## 10. Later

- **A central view**: each server pushes a small daily summary (or the verdict) to a bucket,
  and one page lists all servers.
- **App metrics** from apps that expose Prometheus `/metrics` (Go apps can, with one library).
- **Tracing** (OpenTelemetry) for requests whose time isn't explained by the database or PHP.
