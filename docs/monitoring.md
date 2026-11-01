# Monitoring a server

liops can watch the server it backs up: the machine, the databases' queries, the web
requests, PHP-FPM, Laravel apps and Redis. It answers one question first: **is it the code,
the settings, or the machine?** The design is in [PLAN-monitor.md](PLAN-monitor.md).

Monitoring is off until you turn it on, and a server that only wants backups is unchanged.

## How it runs

- A separate service, `liops-monitor`, as its own unprivileged user. It reads `/proc`, the
  logs it's allowed to read, and the databases' statistics. It never sees liops's config
  or storage credentials. It's capped at 100 MB of memory and 10% of a CPU.
- The UI (the root `liops` service, through your SSH tunnel) asks it over a Unix socket only
  root can open.
- History: every 15 seconds for an hour, every minute for 7 days, every hour for 90 days, in
  `/var/lib/liops-monitor/metrics.db`.

## Turning it on

1. Update liops and run `sudo liops install-system` (it creates the `liops-monitor` user and
   its unit; `scripts/install.sh` does this).
2. In the UI: **Settings → Monitoring**, tick *Monitor this server*, Save. The service starts.
3. Still in Settings → Monitoring, each panel lists what it would change. Nothing changes
   until you press its button:

| Panel | What it changes | Disruption |
|---|---|---|
| Databases | PostgreSQL: the role `liops-monitor` in `pg_monitor` (peer authentication, no password), the `pg_stat_statements` extension, and `shared_preload_libraries` (keeping what's there) in `conf.d/20-liops-monitor.conf`. MySQL: `'liops-monitor'@'localhost'` with `auth_socket` (PROCESS, REFERENCES, `performance_schema`, `sys`). Neither can read the apps' rows. | PostgreSQL needs **one restart** to load `pg_stat_statements`: a button, or the command shown, at a quiet time |
| Web servers | nginx: a JSON `log_format` and `access_log /var/log/nginx/liops-access.log` in `conf.d/liops-monitor.conf`, and one extra `access_log` line in each site that has its own. Apache: a `LogFormat` and `CustomLog ${APACHE_LOG_DIR}/liops-access.log` in `conf-available/liops-monitor.conf` (enabled), and one extra `CustomLog` in each `<VirtualHost>` that has its own. Caddy: `log` in each site without one. | `nginx -t` / `apache2ctl configtest` / `caddy validate`, then a reload; files backed up, put back if the check fails |
| PHP-FPM | `pm.status_path` and a 3 s slow log on pools without them; a read-only OPcache script in `/usr/local/lib/liops`. | `php-fpm8.x -t`, then a reload |
| Redis | An ACL user `liops-monitor` allowed only `INFO`, `PING`, and `LLEN`/`ZCARD` on queue keys. | none (`CONFIG REWRITE` saves it) |

On a server without a browser at hand, `sudo liops monitor-setup` lists the same changes
and `sudo liops monitor-setup --apply` makes them (it never restarts PostgreSQL; it prints
the command). Changed files are backed up under `/var/lib/liops/backups`.

## What to look at

- **Monitoring → Overview**: the verdict for the last 24 hours, most likely cause first, each
  finding with its numbers and what to do. `sudo liops verdict` prints it in the terminal,
  and the daily summary email carries its headline.
- **Database**: the queries that scan whole tables (rows read per row returned), top queries,
  N+1 (with the route that runs them), locks and MySQL's latest deadlock (its two
  statements), tables, unused and duplicate indexes. A query's
  page shows its tables' indexes, a suggested index (SQL and a Laravel migration), and for
  MySQL one real statement.
- **Requests**, **PHP**, **Apps**, **Redis**, **Logs**, **Resources**: the details.
- **Tuning**: recommended settings, each applied only when you press Apply, with a check,
  the least disruptive apply (online or reload), a revert button and a before/after.

Give it a day: most findings compare the last 24 hours, and growth needs six hours.

## Alerts

Besides the daily summary, liops emails straight away (each kind at most once every 6 hours)
when a disk will be full within 3 days or is over 95%, the kernel kills processes for
memory, a burst of 5xx errors happens, or PostgreSQL's `pg_wal` piles up.

## Privacy

Stored: numbers, query shapes (with `?`/`$1` instead of values), routes (never full URLs),
and the top 20 client addresses per hour for 7 days. Read live for a page and never stored,
logged or emailed: running statements, MySQL's sample statement and deadlock report, PHP's slow
log, app errors and log lines. From `.env` only the named settings the checks need are read,
and only the findings leave.

## Turning it off

Settings → Monitoring, untick, Save: the service stops. The database logins, log formats and
pool settings stay (they cost nothing); remove them by hand if you want. `sudo liops
uninstall --purge` removes the monitor's user, unit and history with the rest of liops.
