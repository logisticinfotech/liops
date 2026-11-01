# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

liops is a single static Go binary that runs on a PostgreSQL server (as root, under systemd)
and manages two backup layers to S3-compatible storage: pgBackRest point-in-time backups
(layer 1) and a nightly streamed `pg_dump` per database (layer 2), plus a loopback-only status
UI and SMTP emails. MySQL 8.4 is supported too (`engine: mysql`, `docs/PLAN-mysql.md`): XtraBackup full/diff
plus shipped binlogs (layer 1) and nightly `mysqldump` (layer 2), with the same installer,
wizard and UI. Per-server monitoring is being built (`docs/PLAN-monitor.md`; MO0–MO4 are in: machine, databases, requests/PHP/apps/Redis, the verdict, logs, alerts and tuning). **`docs/PLAN.md` is the design source of truth.** It covers decisions,
the planned package layout (§8.2), and build phases (§9). Read the relevant section before
building a new feature. Phases 0–3 are implemented (engine, UI, installer, wizard); §10
"Status" in PLAN.md says what is still untested. Keep it current.

## Commands

```bash
make test           # go test -race -count=1 ./...
make lint           # gofmt check + go vet (CI fails on unformatted files)
make build          # local binary ./liops
make build-linux    # CGO_ENABLED=0 static linux/amd64 into dist/
make vuln           # govulncheck
make testenv        # Docker: PostgreSQL 18 + pgBackRest + MinIO + Mailpit (TLS), runs test/smoke.sh
make integration    # Docker end-to-end: setup, backups, dumps, emails, dump + PITR restores
make integration-mysql  # Docker end-to-end for MySQL 8.4: setup, dumps, users, restores, emails
make vm-test-mysql  # the same test in a fresh Ubuntu 26.04 Multipass VM, no Docker (Mac)
make vm-test-monitor  # monitoring end to end in a VM: load with known problems, check `liops verdict`
make testenv-down   # tear down and delete volumes
make dev-up         # Mac dev without Docker: MinIO + Mailpit + dev/config.yaml (layout: homebrew)
make dev            # serve against it (LIOPS_DEV=1); see docs/development.md
make css            # rebuild resources/static/app.css (Tailwind standalone CLI, no npm); commit it

go test -run TestUIMustListenOnLoopback ./config   # single test
```

CI (`.github/workflows/ci.yml`) runs gofmt, vet, race tests, the static build, shellcheck,
govulncheck, the Docker smoke test, `test/integration.sh` and `test/integration-mysql.sh`. Releases are built only by `release.yml` on a `v*` tag; version and
commit are injected via `-ldflags -X .../app/version.Version/Commit`.

## Architecture and conventions

- **Plain Go, minimal dependencies, no web framework** (PLAN.md §3). It runs as root and holds
  credentials, so justify every new dependency. The approved stack is listed in PLAN.md §8.1
  (net/http, html/template+embed, robfig/cron, minio-go, go-mail, bbolt, yaml.v3). The folder layout follows Laravel/Goravel conventions (`app/`, `config/`,
  `resources/views/`).
- **CLI** (`main.go`): hand-rolled subcommand dispatch with a `run(args, out)` function, so it's
  testable (`main_test.go`). Each subcommand uses its own `flag.FlagSet`. Keep the `usage`
  string in sync when adding commands.
- **Config** (`config/config.go`): `Parse` decodes YAML *on top of* `Default()` with
  `KnownFields(true)`, so unknown keys are errors. `Validate` collects every problem and returns
  them with `errors.Join`, using `section.field: message` wording. `Load` refuses files readable
  by group/others; `Save` writes atomically (temp file + fsync + rename) with mode 0600. When
  adding a setting: add the field with a yaml tag, a default in `Default()`, validation in
  `Validate()`, and a test.
- **Invariants enforced in code**: the UI must listen on a loopback address only
  (`validateLoopback`); schedules are cron specs interpreted in `server.timezone`; dump settings
  are only validated when `dumps.enabled`.
- **External programs** go through `command.Runner` (`app/command`). DB commands run as the
  `postgres` OS user via `runuser`. Tests use `command.Fake`, `storage.Memory` and
  `notify.Recorder` instead of real programs, S3 and SMTP (see `app/jobs/jobs_test.go`).
- **Jobs** (`app/jobs`): `Runner.Run` takes a file lock (`job.lock` next to the state file),
  so jobs from `serve` and `liops run` queue rather than overlap. Each job is recorded as
  a `state.Run`. `full`/`diff` are followed by a separate `dumps` run. Failures email straight
  away, and a broken state file or a failed email never stops a backup.
- **State** (`app/state`, bbolt) opens the file per transaction, because bbolt allows one
  process at a time and both the service and the CLI use it.
- **Bucket layout**: everything a server stores is under its backup name
  (`cfg.BackupName()` + "/", `storage.Folder`; for PostgreSQL that is `pgbackrest.stanza`). Get a store with `storage.Open(cfg)`, which
  adds the folder, so keys in code stay relative (`dumps/<db>/…`). MySQL binlogs live under
  `xtrabackup/binlog/<generation>/` (a RESET starts a new folder; older backups keep the
  flat `xtrabackup/binlog/<file>.gz`). pgBackRest's
  `repo1-path` is `/<name>/pgbackrest`. Setup claims the folder (`setup.claimFolder`, used
  by `CheckOwner` and `CheckMySQLOwner`): it writes `owners/<engine>-<identity>` and only
  uses a folder that is empty or holds its own marker; anything else is refused before
  anything changes.
- **Dumps** stream `pg_dump` stdout through an `io.Pipe` into a multipart upload; there is no
  local file, and a failed dump leaves no object behind. What is the same for every engine
  (keys, `dumps.Stream`, the pure retention function `dumps.PlanRetention`) is in
  `app/engines/dumps`; an engine (`pgdump`) only implements `jobs.Dumper`. Database names are hex-encoded in discovery
  and passed as `dbname='…'` (`postgres.Conn`), never bare.
- **Service and UI** (`app/service`, `app/http/{controllers,middleware}`, `resources/`):
  `serve` runs `service.Service`, which holds the live config, runner and scheduler. Settings
  saves and the wizard go through it; the scheduler only runs once the config is valid and
  `state` meta `setup_complete` is set (by `liops setup` or the wizard). Templates are
  `resources/views/layout.html` + `partials/*.html` + one page each, embedded with `embed`.
  The strict CSP means no inline scripts or styles. Styles are Tailwind (`resources/css/app.css`
  → `make css` → the committed `resources/static/app.css`; component classes there keep the
  templates' class names short), light and dark (`static/theme.js` sets `class="dark"` before
  the page draws). Charts are uPlot, vendored (no CDN): a page passes its data as JSON in a
  `data-chart` attribute and `static/app.js` draws it. The layout is a sidebar app shell. Controller tests
  (`controllers_test.go`) drive the real handler through `httptest` with a cookie jar and
  fake runners. Use that harness for new pages.
- **No login** in the UI, by decision (PLAN.md §5.2): the SSH tunnel is the access control.
  Keep the protections that cost the user nothing: CSRF tokens, the localhost Host check,
  security headers. Don't add accounts back without asking.
- **Engines** (`engine: postgres | mysql`, default `postgres`): only the chosen engine's
  config sections are validated, and `jobs.New` builds that engine's `Dumper` and layer 1:
  `Runner.Backrest` (pgBackRest) or `Runner.XtraBackup`, never both. Check `cfg.IsMySQL()`
  before anything pgBackRest- or WAL-specific; pages get `page.MySQL`. MySQL base backups
  are folders under `xtrabackup/base/` whose `backup.json` is written last (no manifest =
  failed attempt); binlogs are shipped by the `binlogs` job (own lock, not in the job
history). `liops restore-files` prepares a restore and prints the commands; it never
touches the live server.
  MySQL programs log in with `--defaults-extra-file` (`mysql.Instance.Command`), never with
  a password on the command line.
- **Monitoring** (`app/monitor`, `liops monitor`, `deploy/liops-monitor.service`): a separate
  non-root service (user `liops-monitor`, hardened unit, `MemoryMax=100M`) samples `/proc`,
  `/sys` and cgroups every 15 s, keeps raw samples for an hour in memory and minute/hour
  rollups in its own bbolt file, and answers JSON on a 0600 Unix socket. It never reads
  liops's config or credentials. The root UI reads it with `monitor.Client`; series are named
  `metric` or `metric:label` (`disk.pct:/var`, `svc.mem:nginx.service`). `monitor.enabled` in
  Settings enables and starts the unit (`service.SetMonitor`; tests replace it). Parsers take
  a root directory, so tests use a fake `/proc` tree. What the monitor's user can't read
  (database sizes) root `serve` pushes hourly (`Service.PushDiskUsage` → `POST /v1/push`,
  only `db.size:`/`disk.use:` names); keep that the only write path.
- **Database monitoring** (`app/monitor/db*.go`, `digests.go`, `tables.go`, `analysis.go`): the
  monitor finds PostgreSQL/MySQL by their sockets and runs `psql`/`mysql` as itself
  (`liops-monitor`: peer auth / auth_socket, `pg_monitor` / PROCESS + REFERENCES +
  performance_schema + sys; never the apps' rows). Root creates those logins only when asked
  (`setup.PostgresMonitorAccess`, `setup.MySQLMonitorAccess`, Settings → Monitoring); the
  PostgreSQL restart for `pg_stat_statements` only on a button click. Statement statistics
  are 5-minute differences (shapes only, `?`/`$1`); text that may hold tabs or new lines is
  hex-encoded in SQL. **Real values** (running statements, MySQL's sample statement) are
  read live for a page and never stored, logged or emailed. Pages: `dbmonitor.go`
  (`/monitoring/database`); the daily summary's Server health comes from `jobs.serverHealth`.
- **Requests, PHP, apps** (MO2): the monitor tails access logs (`tail.go`, `reqstore.go`:
  routes, never raw URLs) and reads Redis as its own ACL user (`redis.go`). Root reads what the
  monitor can't: PHP-FPM status/OPcache over FastCGI (`app/php`, pushed every 15 s with a
  short lifetime) and Laravel apps (`app/laravel`: named `.env` keys only, findings out, never
  values; errors grouped live from `storage/logs`). Config changes to nginx, Caddy, PHP-FPM and
  Redis are planned in `app/setup` (`weblogs.go`: nginx, Apache, Caddy; `phpfpm.go`, `redis.go`), shown in Settings,
  and applied only on a button: back up, check (`nginx -t`, `caddy validate`, `php-fpmX.Y -t`),
  put back on failure, reload (never restart). Deploys go into the audit log as chart markers.
- **Verdict, logs, alerts** (MO3): `app/verdict` is pure rules over an `Evidence` struct
  (gathered by `controllers/overview.go` from the monitor, the pools, the apps and the audit
  log); add a rule with a scenario test in `verdict_test.go`. The log viewer (root) reads only
  listed sources, live, the last 2 MB. Immediate alerts (`service/alerts.go`: disk filling,
  OOM kills, a 5xx burst, `pg_wal` piling up) are a pure `judgeAlerts` plus a loop that emails
  each kind at most once every 6 hours.
- **Tuning** (MO4): `app/tuning` recommends (pure, one memory budget); `setup.ApplyTuning`
  applies only `tuning.Allowed` settings into liops's own files (pools: their file, copied),
  checks, applies online/reload, and returns the old values, which revert applies again.
  Never add durability, replication, auth or network settings to `Allowed`. Indexes are
  suggested (`monitor.SuggestIndex`), never created.
- **Layouts** (`postgres.layout`): `debian` (servers, the default) or `homebrew` (Mac
  development). Everything that differs between them (cluster discovery, binary and config
  paths, restart commands) goes through `postgres.Cluster` methods, `pgbackrest.ConfPath`
  and `setup.DetectClusters`. Never hard-code a Debian path elsewhere, and keep the
  `debian` output byte-for-byte unchanged when touching these.
- **Development mode** (`LIOPS_DEV=1`, `dev.go`): no root check, paths default to
  `./dev/`, and `setup --skip-pgbackrest` is allowed. It isn't a production feature: don't
  make server behaviour depend on it. See `docs/development.md`.
- **Setup** steps (`app/setup`) are shared by the CLI and the wizard. Never restart
  PostgreSQL without the user asking for it.
- Plain-language failure reasons come from `notify.LikelyReason` (substring table, first match
  wins). Add patterns there when a new kind of error shows up.

## Public-repo rules

- Every source file starts with an `// SPDX-License-Identifier: Apache-2.0` header (or `#` for
  shell/YAML/Makefile).
- No real hostnames, IPs, bucket names, credentials or email addresses anywhere, including tests
  and examples. Use `example.com` and placeholders. The fixed credentials in `test/compose.yaml`
  are test-only.
- User-facing text (errors, emails, CLI output) is plain language aimed at developers, and says
  what to do next, e.g. the `chmod 600` hint in `Load`.
