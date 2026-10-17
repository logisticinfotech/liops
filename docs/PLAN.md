# pgtijori — Design and build plan

**pgtijori** (*tijori* is Hindi for "safe" or "vault") is a one-stop PostgreSQL backup tool
for a single database server. It packages a proven manual setup (pgBackRest point-in-time
backups to S3-compatible storage, plus per-database dumps, alerts and a status page) as one Go
binary with an install command, a setup wizard, a status UI with downloads, and
plain-language emails.

Nothing in this repository is specific to any particular deployment. Each server's details
(hostnames, buckets, credentials, recipients) live only in that server's config file.

---

## 1. Goals and non-goals

**Goals**

- **One command to install** on a server. Everything else is done from a web setup wizard.
- **Two backup layers per server** (§4):
  1. **Point-in-time backups** (pgBackRest: weekly full, nightly differential, WAL archived at
     least hourly) to object storage. This is for disaster recovery.
  2. **A nightly `pg_dump` of each database**, found automatically. **On by default**, and can
     be turned off per server or per database. The dumps are downloadable files, used for
     restoring or exporting a single database (e.g. one tenant of a multitenant app).
- A **status UI** for each server: health, backup history, storage, and per-database dump
  **downloads**.
- **Plain-language emails**: a daily summary and immediate failure alerts.
- **Pluggable storage**: S3-compatible first (Cloudflare R2 is the reference target).
  GCS and Google Drive can be added later without a rewrite.

**Non-goals (v1)**

- **Restoring from the UI.** Restores are manual, using pgBackRest and `pg_restore` over SSH,
  following a documented runbook (`docs/restore.md`, Phase 3). The UI shows the commands for a
  dump, filled in, but never runs them.
- **A central dashboard for many servers.** v1 has one UI per server.
- **Automatic restore tests.** Restore tests are manual, following the runbook.
- **Browser access over the internet.** The UI is reached through an SSH tunnel only.
- **Encryption of backups.** v1 relies on a private bucket and a bucket-scoped token. Client-side
  encryption may be added later as an option.
- Backing up anything other than PostgreSQL (app files, server config).

---

## 2. Decisions

| Topic | Decision |
|-------|----------|
| Name | **pgtijori**: binary `pgtijori`, service `pgtijori.service` |
| Scope | Any PostgreSQL server: single-database apps and database-per-tenant apps alike |
| Code and releases | Public repo [`logisticinfotech/pgtijori`](https://github.com/logisticinfotech/pgtijori), with GitHub Releases for the binary and `install.sh` (§7.1) |
| License | **Apache-2.0** |
| Users | Developers. Emails and UI are plain, but don't need to be non-technical. |
| Restore from UI | **No.** Restores are manual, using the runbook. An in-place restore button was built and removed (October 2026): with no login (§5.2) any local process could replace a live database, and swapping a database is the moment a person has to stop the app. The UI shows filled-in commands instead. |
| Restore tests | Manual, using the runbook |
| UI | **One per server**, no central dashboard in v1 |
| UI access | **SSH tunnel only**, and **no login** (§5.2). The UI binds to a loopback address, enforced by config validation (§5.3). |
| Emails | A daily summary every day, plus immediate failure emails |
| Storage | S3-compatible (R2, AWS S3, B2, Wasabi, MinIO). Recommended: **one private bucket per server**, each with its own token scoped to that bucket, so a compromised server can't touch other servers' backups. |
| Point-in-time retention | 30 days (default) |
| Per-database dumps | On by default; 7 days retention (default) |
| Deleted database | Keep its last dump 14 days (default), then delete it |
| Language | **Plain Go**, no web framework (§3) |

---

## 3. Why plain Go, with no framework

- It must be **one file** to copy to any server, with no `.env`, framework config tree or
  service providers.
- It must **keep working when PostgreSQL is down**. Its own state is a small embedded file,
  not a database.
- It **runs as root and holds database and storage credentials**. Few dependencies means less to
  audit and patch.
- It is installed on **servers that are rarely touched**. Go's standard library stays
  compatible, while framework minor versions can break.
- A full web framework's main features (ORM, migrations, queues, multi-DB) would sit unused.

The folder layout follows Laravel/Goravel conventions (`app/`, `config/`, `resources/views/`),
so it feels familiar to developers coming from those frameworks.

---

## 4. Architecture

```
 One server
 ┌──────────────────────────────────────────────────────────────────────┐
 │ PostgreSQL cluster                                                   │
 │   app DB · tenant_abc · tenant_xyz · …                               │
 │      │ WAL (archive_command)              ▲ pg_dump (read-only)      │
 │      ▼                                    │                          │
 │   pgBackRest ◄────── runs ────────── pgtijori (systemd, Go)          │
 │      │                                    │  scheduler · jobs · state │
 │      │                                    │  UI on 127.0.0.1:8899     │
 │      │                                    │  SMTP                     │
 └──────┼────────────────────────────────────┼──────────────────────────┘
        ▼                                    ▼ (streamed, no local file)
   S3-compatible bucket (one per server)
     pgbackrest/                  ← layer 1: full + diff + WAL (point-in-time)
     dumps/<database>/<date>.dump ← layer 2: nightly pg_dump per database
```

### 4.1 Layer 1: point-in-time backups (pgBackRest)

- One **stanza per server**. It covers **every database in the cluster**, all captured at the
  same moment, so they stay consistent. New databases are included automatically.
- Weekly full, nightly differential, `archive_timeout = 1h` (configurable, 1m–24h), 30-day
  retention, zstd.
- pgtijori writes `pgbackrest.conf` and the PostgreSQL `conf.d` file, and runs `backup`, `check`
  and `info --output=json`. pgBackRest does the actual work. pgtijori doesn't reimplement
  backups.

### 4.2 Layer 2: per-database dumps (`pg_dump`)

- **Discovery** every night: `SELECT datname FROM pg_database WHERE NOT datistemplate`, minus
  exclusions (default: `postgres`). New databases are picked up automatically.
- **Dump**: `pg_dump -Fc -Z zstd:3 <db>`, with stdout **streamed straight to storage** through a
  multipart upload. No dump file is written to the server's disk. Set the upload part size
  explicitly (e.g. 16 MiB) to bound memory use. Also `pg_dumpall --globals-only` (roles) once a
  night.
- **Order**: dumps run **after** the nightly backup, one database at a time (`concurrency`,
  default 1).
- **Retention**: keep N days per database (default 7). pgtijori lists `dumps/<db>/` and deletes
  older objects.
- **Deleted databases**: when a database disappears from discovery, pgtijori marks it
  `deleted since <date>`, sends an email, keeps its **newest** dump for N days (default 14),
  then deletes it. Note: layer 1 still contains the deleted database until its backups
  expire. Operators should reflect that in their privacy policy.
- **Labels (optional)**: a configurable SQL query against one database that returns
  `(database name, label)`, e.g. `SELECT db_name, subdomain FROM tenants`. The UI and emails
  then show `abc` instead of `tenant_abc`.

> Dumps of different databases are taken at different moments. They're for single-database
> restores and exports. For a restore where everything must be consistent, use layer 1.

### 4.3 Storage interface

```go
type Store interface {
    Put(ctx context.Context, key string, r io.Reader) (size int64, err error) // streaming
    List(ctx context.Context, prefix string) ([]Object, error)
    Delete(ctx context.Context, key string) error
    DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error)
    Probe(ctx context.Context) error // write/read/delete a test object (wizard + daily check)
}
```

- **v1: S3-compatible** (`minio-go`): Cloudflare R2, AWS S3, Backblaze B2, Wasabi, MinIO.
- **Later**: GCS. Google Drive for **dumps only**. Drive can't hold layer 1, because pgBackRest
  doesn't support it and Drive's request limits don't suit WAL.
- Layer 1 storage is whatever pgBackRest supports (`s3`, `gcs`, `sftp`, `posix`). pgtijori only
  writes the matching `repo1-*` settings.

### 4.4 Scheduler and jobs

Built-in scheduler (`robfig/cron`) with an **explicit timezone** (`server.timezone`), so
schedules can be written in local time while the server clock stays on UTC.

| Job | Default | What it does |
|-----|---------|--------------|
| `full` | Sun 01:30 | `pgbackrest backup --type=full` |
| `diff` | Mon–Sat 01:30 | `pgbackrest backup --type=diff` |
| `dumps` | right after `full`/`diff` | discovery → dump each DB → retention → deleted-DB handling |
| `report` | daily 07:30 | `pgbackrest check` + storage probe → daily summary email |

- **One job at a time per server** (in-process lock). A job that would overlap is queued, not
  dropped.
- Every run is recorded (start, end, status, size, short output). The full output goes to the
  log file.
- Any failed job sends a **failure email straight away**.

### 4.5 Process, files and permissions

| Item | Value |
|------|-------|
| Service | systemd unit `pgtijori.service`, runs as **root**, runs DB commands as `postgres` (`runuser -u postgres --`) |
| Why root | The wizard has to write `/etc/pgbackrest`, write `/etc/postgresql/.../conf.d` and restart PostgreSQL. After setup it only needs `postgres`. Root keeps v1 simple; revisit later. |
| Config | `/etc/pgtijori/config.yaml`, mode `0600`, holds secrets. Refused if group or others can read it. |
| State | `/var/lib/pgtijori/state.db` (bbolt, pure Go): job history (90 days), audit log, deleted-DB tracking, UI users |
| Logs | `/var/log/pgtijori/pgtijori.log` (+ logrotate) |
| UI | Listens on a **loopback address only** (default `127.0.0.1:8899`), never on a public port |

---

## 5. UI (one per server)

Server-rendered pages (`html/template` + `embed`) with a little vanilla JS. Six screens:

| Screen | Content |
|--------|---------|
| **Dashboard** | Overall health (OK / ATTENTION / FAILED), last full and differential, **can restore back to `<date>`**, newest WAL upload, storage used, server disk, next scheduled jobs |
| **Point-in-time backups** | Table from `pgbackrest info --output=json`: type, start/end, duration, size |
| **Databases** | One row per database: label, last dump time and size, history, **Download** button. Deleted databases show "deleted since … (dump kept until …)". |
| **Jobs** | History of all runs, filterable, with each job's output. A **Run now** button for `diff`, `dumps` and `report`. |
| **Settings** | Storage, email, schedules, retention, exclusions, label query. Each section has a **Test** button. |
| **Audit log** | Which dump was downloaded, which settings changed, which jobs were started, and when |

### 5.1 Downloads

- **Download** creates a **short-lived presigned URL** (10 minutes), so the browser downloads
  straight from storage and the server isn't in the path. Every download is written to the
  audit log.
- Downloads cover **layer 2 dumps** only. Layer 1 files aren't usable on their own.

### 5.2 No login

There are no UI accounts: being able to open the SSH tunnel is the access control. This was
decided after trying the first version, where a setup token, a password and a TOTP code in
front of a page that's only reachable over SSH was too much friction.

The trade-off, accepted on purpose: anything that can connect to `127.0.0.1:8899` **on the
server** can use the UI, including downloading dumps and changing settings. That means every
local user and process, for example a web app on the same server with an SSRF bug. If that
becomes a concern, the planned fix is to listen on a Unix socket that only the `sudo` group
can open (`ssh -L 8899:/run/pgtijori/ui.sock …`), which keeps the same convenience.

Still in place, because they cost the user nothing: CSRF tokens on every form and an Origin
check, SameSite=Strict cookies, a strict Content-Security-Policy, and refusing requests whose
`Host` isn't localhost (DNS rebinding). Downloads and settings changes are in the audit log.

### 5.3 Access

The UI listens on a loopback address only and is reached through an **SSH tunnel**:

```bash
ssh -L 8899:127.0.0.1:8899 <user>@<server>
# then open http://localhost:8899
```

For several servers at once, give each tunnel its own local port. No firewall port is ever
opened for pgtijori. Config validation rejects any non-loopback `ui.listen`.

---

## 6. Emails

Any SMTP account works (e.g. Gmail with an app password). SMTP is built in, with no `msmtp`
or local MTA. Plain text; a simple HTML version is optional.

**Daily summary:**

```
Subject: ✅ db-1 — backups healthy — Mon 5 Oct

All good. No action needed.

Point-in-time backup:  last diff today 01:30 (36 s) · can restore back to 5 Sep
Newest change saved:   today 07:00
Database dumps:        11 of 11 databases dumped tonight (total 412 MB)
Storage:               1.2 GB used
Server disk:           18% used

Next full backup: Sun 11 Oct 01:30
```

**Failure** (sent straight away):

```
Subject: ❌ db-2 — tonight's database dumps FAILED for 1 database

What failed:    dump of tenant_xyz ("xyz")
Likely reason:  the database refused the connection (is PostgreSQL running?)
Still safe:     point-in-time backups are fine; tenant_xyz's last good dump is from 4 Oct
What to do:     check the server, then press "Run now → dumps" in the backup UI

--- technical details ---
<exit code, last 30 lines of output>
```

**Other emails:** setup finished, new database found (first dump done), database deleted
(dump kept until …).

Known errors are translated into a plain "Likely reason" line, for example: HTTP 403 → "the
storage key was rejected"; timeout → "couldn't reach storage"; `No space left` → "the server
disk is full"; `archive ... not archived before timeout` → "WAL upload is stuck". Anything
unrecognised shows "unknown — see technical details".

---

## 7. Install and setup wizard

### 7.1 One command

```bash
curl -fsSL https://github.com/logisticinfotech/pgtijori/releases/latest/download/install.sh | sudo bash
```

**Releases:** a GitHub Actions workflow builds each tagged version (`v1.2.0`) and attaches
`pgtijori-linux-amd64`, `install.sh` and `SHA256SUMS` to the GitHub Release. To pin a version,
use `…/releases/download/v1.2.0/install.sh`.

Because the repository is public:

- **No secrets, hostnames, IPs, bucket names or email addresses** in the repo, not even in
  examples or tests. Use `example.com` and placeholders.
- Apache-2.0: `LICENSE` in the root and an SPDX header in each source file.
- Repository settings: 2FA required for members, `main` protected (PR + green CI), releases
  created only by the workflow.
- `install.sh` verifies the binary against `SHA256SUMS`.
- Dependabot and `govulncheck` run in CI, because the tool runs as root.

`install.sh`:

1. Checks: Ubuntu, systemd, PostgreSQL installed (`pg_lsclusters`), root.
2. Installs `pgbackrest` from PGDG (adds the repository if it's missing).
3. Downloads the `pgtijori` binary (linux/amd64, static) and checks its SHA-256.
4. Creates the directories, systemd unit and logrotate file, then starts the service.
5. Prints the SSH-tunnel command to open the wizard.

Re-running it **upgrades** the binary and keeps the config. `pgtijori uninstall` removes
pgtijori, but leaves the backups in storage untouched.

### 7.2 Wizard (in the browser)

1. **PostgreSQL:** pick the cluster (auto-detected: version, data dir, port), then show the
   database list.
2. **Storage:** endpoint, bucket, key and secret. **Test** writes, reads and deletes a probe
   object.
3. **Email:** SMTP account, recipients. **Test** sends an email.
4. **Schedules and retention:** defaults from §4.4 and §2, editable.
5. **Labels (optional):** label database and query. **Test** shows the mapping.
6. **Review and apply:**
   - writes `pgbackrest.conf` and `conf.d/10-pgbackrest.conf`;
   - **asks before** restarting PostgreSQL (seconds of downtime, needed for `archive_mode`),
     or lets you schedule the restart for later;
   - runs `stanza-create`, `check`, the **first full backup** and the **first dumps**,
     showing live progress;
   - sends the "setup finished" email.

If a step fails, the wizard stops there with the error and a **Retry** button. Every step is
safe to run again.

### 7.3 CLI (for SSH use)

`pgtijori status | run <full|diff|dumps|report> | logs | version | config init|validate | uninstall`.
Restores use `pgbackrest` and `pg_restore` directly (see `docs/restore.md`).

---

## 8. Tech stack and code layout

### 8.1 Stack

| Need | Choice |
|------|--------|
| Language | Go (see `go.mod`), `CGO_ENABLED=0`, static linux/amd64 binary |
| HTTP / routing | Standard library `net/http` (method + path patterns) |
| Templates / assets | `html/template` + `embed` |
| Scheduler | `github.com/robfig/cron/v3` (with location) |
| Object storage | `github.com/minio/minio-go/v7` |
| SMTP | `github.com/wneessen/go-mail` |
| State | `go.etcd.io/bbolt` |
| Config | `gopkg.in/yaml.v3` |

### 8.2 Layout

```
pgtijori/
  main.go                  # small CLI: serve | status | run | config | ...
  app/
    version/               # build info (set by ldflags)
    jobs/                  # full, diff, dumps, report, retention
    engines/pgbackrest/    # write conf, run commands, parse info JSON
    engines/pgdump/        # discovery, streamed dump, retention, deleted DBs
    storage/               # Store interface + S3 implementation
    notify/                # email templates + error → "likely reason" mapping
    http/controllers/      # dashboard, backups, databases, jobs, settings, wizard
    http/middleware/       # sessions, csrf, Host check, security headers
    state/                 # bbolt repositories
  config/                  # config struct, load/save, defaults, validation
  resources/views/         # templates (embedded)
  scripts/install.sh
  deploy/                  # systemd unit, logrotate
  docs/                    # this plan, restore runbook
  test/                    # Docker test environment
```

### 8.3 Testing

- Unit tests for config validation, `pgbackrest info` JSON parsing, retention rules, error
  mapping and deleted-DB logic.
- **Integration test in Docker** (`test/`): Ubuntu 26.04 + PostgreSQL 18 + pgBackRest + MinIO
  (over TLS). Run the wizard through `apply`, then backups and dumps, and finally **restore** a
  dump and a pgBackRest backup to prove they work. Runs in CI.

---

## 9. Build phases

Rough estimates for one developer. Each phase ends with a usable result.

| Phase | Deliverable | Done when | Estimate |
|-------|-------------|-----------|----------|
| **0. Skeleton** | License, README, layout, config load/save/validate, CLI (`version`, `config`), CI (gofmt, vet, tests, static build, `govulncheck`, test environment), release workflow, Docker test environment | A tag produces a GitHub Release, and CI starts the test environment | 2 days |
| **1. Engine + CLI + emails** | Layer 1 jobs, layer 2 dumps (streamed, retention, deleted DBs), scheduler, state, daily and failure emails. Configured by hand-written `config.yaml`. | Integration test passes (backups, dumps, restore of both). Runs for a week on a test VM. | 1–1.5 weeks |
| **2. UI** | Dashboard, backups, databases + downloads, jobs + Run now, settings, audit log | All screens work against the test environment; downloads are audited | 1 week |
| **3. Installer + wizard + docs** | `install.sh`, wizard steps 1–6, CLI, upgrade/uninstall, `docs/restore.md` | A fresh Ubuntu VM goes from nothing to its first backup using only the one command and the wizard | 1 week |
| **Later** | Google Drive for dumps, GCS, optional encryption, central dashboard | — | — |

---

## 10. Status

- **Phase 0:** implemented. Waiting for its first CI run on GitHub (the Docker test environment
  has only run in CI, not locally).
- **Phase 1:** implemented on branch `phase-1-engine`: `setup`, `run`, `status`, `serve`, all four
  jobs, streamed dumps with retention and deleted-database handling, and the emails. The
  integration test (`test/integration.sh`: setup, backups, dumps, a dump restore and a
  point-in-time restore, emails) still has to pass in CI, then run for a week on a test VM.
  Notes from building it:
  - Dump keys are `dumps/<db>/<UTC time>.dump`, with the time to the second so a manual
    "Run now" doesn't overwrite the night's dump. Roles go to `globals/<time>.sql`.
  - Retention always keeps a live database's newest dump, even past the retention period, so
    failed nights can't leave a database with no dump.
  - A database added to `dumps.exclude` is treated like a deleted one.
  - The job lock is a file lock (`/var/lib/pgtijori/job.lock`), so `pgtijori run` over SSH
    queues behind a scheduled job instead of overlapping it.
  - `email.tls` is `starttls` or `tls`; plaintext SMTP isn't allowed.
- **Phase 2:** implemented on branch `phase-2-ui`: all six screens, audited downloads, settings
  that apply without a restart, schedules picked as day + time (custom cron still accepted),
  a progress bar for running jobs, and filled-in restore commands for each database. Login (password + TOTP) was built, then removed after a
  first try on Ubuntu (§5.2). Notes:
  - Requests whose `Host` isn't localhost are refused (DNS rebinding), on top of
    loopback-only listening.
  - "Run now" is offered for `diff`, `dumps` and `report`. A full backup by hand is
    `sudo pgtijori run full`.
- **Phase 3:** implemented on branch `phase-3-installer`: `scripts/install.sh`,
  `install-system`, the wizard, `uninstall`, `logs`, `dumps list|url`,
  and [restore.md](restore.md). Not yet done: the "done when" test, a fresh Ubuntu VM taken
  from nothing to its first backup with only the installer and the wizard.
- **Next:** test on a real Ubuntu server: Phase 1's week-long run and Phase 3's fresh-VM
  install.
