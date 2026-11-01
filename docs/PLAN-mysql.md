# liops for MySQL: design and build plan

Status: **built** (M0–M3, October 2026). [PLAN.md](PLAN.md) stays the
source of truth for everything this document doesn't change; read it first. §10 says what is tested.

## 1. Requirement

- Back up **MySQL 8.4 LTS and newer 8.4.x** servers with the same two layers, UI and emails as
  PostgreSQL servers.
- MySQL runs on **its own machines**, never on the same machine as a PostgreSQL server that
  liops backs up. One liops install backs up exactly one engine.
- **Point-in-time restore is required.**
- Starting size: about 1 GB of data (about 100 MB compressed). The design has to work well at
  that size, and keep working at tens of GB.

Not in scope: MySQL 9.x Innovation releases, MariaDB, Group Replication / InnoDB Cluster,
restoring from the UI (same rule as PLAN.md §2).

## 2. Decisions

| Topic | Decision |
|-------|----------|
| Engine choice | New top-level `engine: postgres \| mysql`, default `postgres`, so existing config files keep working unchanged. |
| Layer 1 tool | **Percona XtraBackup 8.4** (`percona-xtrabackup-84`, Percona's apt repository). It's the standard hot physical backup for InnoDB, and it streams. MySQL Enterprise Backup needs a paid licence. |
| Layer 1 shape | Weekly full, nightly differential (incremental from the last *full*, so a restore is always full + at most one diff), plus **binlog shipping** for the moments in between. |
| Who does layer 1 | **liops itself.** There's no pgBackRest for MySQL, so liops writes the base backups, binlogs, manifests and retention. This is the biggest difference from the PostgreSQL side. |
| Recovery point | Binlogs are shipped every `binlog_interval` (default **5 minutes**, configurable 1m–1h). Worst case you lose that much on a total loss of the server. Zero-loss streaming is a later option (§9). |
| GTIDs | **Required** (`gtid_mode=ON`, `enforce_gtid_consistency=ON`). Replay then skips transactions the base backup already has, and a bad transaction (e.g. an accidental `DROP TABLE`) can be skipped by its GTID. Both can be turned on **without a restart**, but **setup asks first** (decided October 2026), like the PostgreSQL restart: `enforce_gtid_consistency` rejects a few kinds of statement an app might use, so a person decides when. |
| Layer 2 tool | `mysqldump --single-transaction`, gzipped in Go (standard library), streamed to storage like `pg_dump` today. |
| MySQL login | A dedicated MySQL user `liops@localhost` with only the privileges it needs. Its password is kept in `/etc/liops/mysql.cnf` (mode 0600, root) and passed with `--defaults-extra-file`, never on a command line. |
| Restores | Manual, following a runbook (`docs/restore-mysql.md`). A `liops` helper downloads and prepares the files into an empty directory and prints the remaining commands. It never touches the live server. |
| Name | Keep `liops` for now. Renaming is cosmetic and can wait. |

## 3. Architecture

```
 One MySQL server
 ┌──────────────────────────────────────────────────────────────────────┐
 │ MySQL 8.4 (InnoDB, GTIDs on, binlogs on)                             │
 │   app · tenant_abc · …                                               │
 │      │ binlog files              ▲ xtrabackup / mysqldump (read-only) │
 │      ▼                           │                                   │
 │   liops (systemd, Go) ────────┘  scheduler · jobs · state · UI    │
 └──────┼───────────────────────────────────────────────────────────────┘
        ▼ (streamed, no local copy)
   S3-compatible bucket
     <backup name>/
       owners/mysql-<server_uuid>                ← marker: which server uses this backup name (M1)
       xtrabackup/base/<UTC time>-full/
         backup.xbstream                         ← xtrabackup --stream, zstd-compressed
         backup.json                             ← manifest, written LAST (no manifest = incomplete)
       xtrabackup/base/<UTC time>-diff/…         ← incremental from that week's full
       xtrabackup/binlog/binlog.000123.gz        ← every closed binlog file
       dumps/<database>/<UTC time>.sql.gz        ← layer 2
       globals/<UTC time>.sql                    ← users and grants
```

### 3.1 Layer 1: base backups

```
xtrabackup --defaults-extra-file=/etc/liops/mysql.cnf --backup \
  --stream=xbstream --compress=zstd --parallel=4 \
  --extra-lsndir=/var/lib/liops/xtrabackup/<id> --target-dir=/var/lib/liops/xtrabackup/<id> \
  [--incremental-lsn=<to_lsn of the last full>]
```

- stdout goes through `io.Pipe` into a multipart upload, as dumps do today. Only the small
  `xtrabackup_checkpoints` / `xtrabackup_info` files touch the disk (`--extra-lsndir`).
- When the upload succeeds, liops writes `backup.json`: type, start and stop time, LSNs,
  binlog file and position, GTID set, the full it is based on, XtraBackup and MySQL versions,
  and size. Everything that reads backups only trusts folders that have a manifest. Folders
  without one are failed attempts, and retention deletes them.
- A diff whose full is missing or has no manifest is refused, and the job falls back to a full.
- Before each backup: check that the installed XtraBackup supports the running MySQL
  version. XtraBackup refuses a server newer than itself, so a `apt upgrade` of MySQL
  without XtraBackup would otherwise break backups. The daily report checks this too.

### 3.2 Layer 1: binlog shipping

- A new `binlogs` job runs every `binlog_interval`:
  1. `SHOW BINARY LOG STATUS` (the 8.4 name; `SHOW MASTER STATUS` is gone). If the position
     moved since the last run, `FLUSH BINARY LOGS` closes the current file.
  2. `SHOW BINARY LOGS` lists the files. Every **closed** file not yet in storage is gzipped and
     uploaded. Its name, size and GTID range go into the state file.
- It uses its **own lock** (`binlog.lock`), so a long full backup doesn't hold up shipping.
  Binlog files only grow and are never rewritten, so shipping and backups don't conflict.
- Successful runs are **not** recorded as job runs (288 a day would bury the history). Only
  the last success and any failures are. A failure emails straight away, like other jobs.
- **Gap detection:** file numbers and GTID sets must follow on from each other, starting at
  the binlog position recorded in the oldest kept base backup. A gap means a file was purged
  before it was shipped, or someone ran `RESET BINARY LOGS AND GTIDS`. liops emails, says
  which window can't be restored, and takes a new full backup straight away so the chain starts
  again.
- Setup checks that `binlog_expire_logs_seconds` is at least 7 days, so MySQL keeps local
  binlogs long enough to survive a storage outage of a few days.

### 3.3 Layer 1: retention

- Pure function, like `dumps.PlanRetention`: given manifests, binlogs and `retention_days`
  (default 30), keep every full newer than the window **plus the newest full older than it**,
  so the whole window can be restored. Diffs go with their full. Binlogs older than the oldest
  kept base backup's start position are deleted.

### 3.4 Layer 2: per-database dumps

- **Discovery:** `SELECT HEX(schema_name) FROM information_schema.schemata`, minus `mysql`,
  `sys`, `information_schema`, `performance_schema` (always) and `dumps.exclude`.
  The names are hex-encoded, as on the PostgreSQL side.
- **Dump:** `mysqldump --single-transaction --quick --routines --triggers --events --hex-blob
  --set-gtid-purged=OFF --no-tablespaces <db>`, gzipped in Go, streamed to storage.
  `--set-gtid-purged=OFF` makes a dump loadable into any server, which is what layer 2 is for.
- **Users and grants** (the `pg_dumpall --globals-only` equivalent): `mysqlpump` was removed in
  8.4, so liops writes them itself from `SHOW CREATE USER` + `SHOW GRANTS`.
- Retention, deleted databases and labels: unchanged. They're already engine-agnostic once the key
  parser accepts `.sql.gz`.

### 3.5 Restore (manual, runbook)

`liops restore-files --to "2026-10-28 14:05" --dir /restore/empty` (new CLI command):

1. Picks the newest full (+ diff) taken before the target, and the binlogs after it.
2. Downloads them, runs `xbstream -x`, `xtrabackup --decompress`, then
   `--prepare --apply-log-only` (full), `--prepare --incremental-dir` (diff), `--prepare`.
3. Prints the rest, filled in, for a person to run: stop MySQL, move the old datadir
   aside, `xtrabackup --copy-back`, `chown -R mysql:mysql`, start MySQL, then
   `mysqlbinlog --start-position=<from the manifest> --stop-datetime="<target>" <first binlog>`
   followed by the later binlogs without `--start-position`, piped into `mysql`.
   On Ubuntu the restore goes **in place** of `/var/lib/mysql`: the `mysqld` AppArmor profile
   only allows the socket names `mysqld.sock`/`mysqlx.sock`, so a second server next to the
   live one can't start without changing AppArmor (found in M2's spike).
   To undo one bad statement instead of stopping before it, the runbook shows
   `--exclude-gtids=<gtid of the DROP>`.

Dump restore: `gunzip -c <file>.sql.gz | mysql <db>`, shown filled in on the Databases page.

## 4. MySQL settings and privileges

Checked by setup, and changed only after asking (like the PostgreSQL restart today):

| Setting | Needed | Restart? |
|---------|--------|----------|
| `log_bin` | ON (8.4 default) | yes if it's off |
| `binlog_format` | ROW (8.4 default) | no |
| `gtid_mode`, `enforce_gtid_consistency` | ON | **no**: the documented online steps (`OFF_PERMISSIVE` → `ON_PERMISSIVE` → `ON`) with `SET GLOBAL`, **and** written to `/etc/mysql/mysql.conf.d/zz-liops.cnf`. Not `SET PERSIST` alone: it saves to `mysqld-auto.cnf` in the data directory, which a restored backup doesn't have (found in M2's spike), so a restored server would come up with GTIDs off and replay would fail. |
| `binlog_expire_logs_seconds` | ≥ 604800 (7 days) | no |
| `sync_binlog`, `innodb_flush_log_at_trx_commit` | 1 (8.4 defaults) | no; warn only |
| Storage engine | InnoDB for all user tables; warn about MyISAM (it's locked during backup and isn't crash-safe) | — |

The `liops@localhost` user gets: `BACKUP_ADMIN, PROCESS, RELOAD, LOCK TABLES, REPLICATION
CLIENT, SELECT, SHOW VIEW, TRIGGER, EVENT, SHOW_ROUTINE` and `SELECT` on
`performance_schema.log_status` and `performance_schema.keyring_component_status`. Check this
list against XtraBackup 8.4's documentation during Phase M2. Setup creates the user by
connecting as MySQL's `root`, by socket auth if possible, otherwise with a password asked for
once and never stored (§9).

## 5. Config

```yaml
engine: mysql
mysql:
  backup_name: shop-db-1                   # folder in the bucket (pgbackrest.stanza's counterpart)
  socket: /var/run/mysqld/mysqld.sock
  defaults_file: /etc/liops/mysql.cnf      # written by setup, root only, 0600
  data_dir: /var/lib/mysql
  bin_dir: ""                              # empty: /usr/bin
  full_schedule: "30 1 * * 0"              # nightly backup times; until M2 the dumps run then
  diff_schedule: "30 1 * * 1-6"
  # M2 adds: retention_days, binlog_interval
dumps: …          # unchanged; MySQL's own schemas are always skipped
```

- Everything for MySQL is in one `mysql:` section (decided in M1, October 2026): liops is
  the MySQL backup tool itself, so a separate `xtrabackup:` section, as first planned, would
  only split one thing in two.
- `postgres:` / `pgbackrest:` are only validated when `engine: postgres`, and `mysql:` only
  when `engine: mysql`. `engine` defaults to `postgres`, so existing config files keep working.
- `cfg.BackupName()` returns `pgbackrest.stanza` or `mysql.backup_name`. `storage.Folder` and
  everything else use it.

## 6. Code changes

| Area | Change |
|------|--------|
| `app/jobs` | `Runner` holds a layer-1 interface (`Backup`, `Check`, `Info`) and a dumper interface (`Discover`, `Labels`, `Dump`, `DumpGlobals`) instead of `*pgbackrest.Engine` / `*pgdump.Engine`. New `binlogs` job. |
| `app/engines/mysql/` | Instance: binary paths, `Command`, `Query` (`mysql -N -B` with `--defaults-extra-file`), `Setting`. The counterpart of `engines/postgres`. |
| `app/engines/xtrabackup/` | Streamed backups, manifests, binlog shipping, gap check, `PlanRetention`, info for the UI. |
| `app/engines/mysqldump/` | Discovery, dump, users and grants, implementing `jobs.Dumper`. Keys, streaming and retention come from `app/engines/dumps` (done in M0). |
| `app/setup` | `mysql.go`: detect MySQL, create the user and `mysql.cnf`, check and enable settings, claiming the folder by `server_uuid` (shared rule in `app/setup/owner.go`). |
| Wizard, UI, emails | Step 1 picks the engine (auto-detected). Backups page shows base backups and "can restore to any moment from X to Y, last binlog shipped N minutes ago". Wording per engine ("binlogs" vs "WAL", "users and grants" vs "roles"). |
| `notify.LikelyReason` | MySQL and XtraBackup errors: access denied, the server-newer-than-XtraBackup error, purged binlog, lock wait timeout. |
| `scripts/install.sh` | If `mysqld` is present (and PostgreSQL isn't), install `percona-xtrabackup-84` from Percona's repository instead of pgBackRest. |
| `main.go` | `run binlogs`, `restore-files`; `usage` updated. |
| Tests / CI | `test/` gets a MySQL 8.4 + XtraBackup + MinIO + Mailpit compose file and a MySQL integration script (§7). |
| Mac development | Dumps work against Homebrew `mysql@8.4`. XtraBackup doesn't support macOS, so layer 1 is developed against Docker only. |

No new Go dependencies: gzip and the MySQL client programs cover everything. liops calls
`mysql`, `mysqldump`, `mysqlbinlog` and `xtrabackup` through `command.Runner`, like it calls
`psql` today, so there's no MySQL driver either.

## 7. How we know point-in-time restore works

"Perfect" PITR is something we prove with a test, not something we assume. The CI integration
test for MySQL:

1. Load data, take a full. Write more, take a diff. Write more, ship binlogs.
2. Record `CHECKSUM TABLE` for every table and the time T. Then `DROP TABLE` something and ship
   again.
3. In a **fresh container**: `restore-files --to T`, follow the printed commands, compare
   checksums with step 2. They must match exactly.
4. Repeat, skipping only the `DROP` by its GTID, and check that the writes after it survived.
5. Purge a binlog before shipping. The gap must be reported and a new full taken.

Then a week on a test VM with real traffic, and one restore drill on it, before production.

## 8. Build phases

| Phase | Deliverable | Done when | Estimate |
|-------|-------------|-----------|----------|
| **M0. Engine split** | `cfg.BackupName()`, `jobs.Dumper`, shared `app/engines/dumps` (keys, streaming, retention). **Done** (October 2026). The `engine` setting moves to M1 and the layer-1 interface to M2, where a second implementation shapes them. | All existing tests pass; PostgreSQL output byte-for-byte unchanged | 2–3 days |
| **M1. MySQL dumps** | `engines/mysql`, `engines/mysqldump`, users and grants, MySQL settings in config, CLI `setup`, Docker test environment. **Built** (October 2026); the integration test passed in an Ubuntu VM, see §10. | Dumps run, expire, and restore in the Docker test | 1 week |
| **M2. Point-in-time** | XtraBackup full/diff streaming, manifests, binlog shipping + gap check, retention, report checks, `restore-files`, `docs/restore-mysql.md`. **Built** (October 2026); the restore test passed in an Ubuntu VM, see §10. | §7 passes in CI | 2 weeks |
| **M3. UI, wizard, installer** | Engine-aware wizard, pages and emails, `install.sh` for MySQL. **Built** (October 2026); passed in an Ubuntu VM, see §10. | A fresh Ubuntu VM with MySQL 8.4 goes from nothing to its first backup with the installer and wizard | 1 week |

## 9. Risks and open questions

- **XtraBackup lags MySQL releases.** Percona usually ships XtraBackup for a new 8.4.x within
  weeks. Until then, backups of an upgraded server fail. Mitigation: the version check in
  §3.1, a daily-report warning, and a runbook note to hold back `mysql-server` with
  `apt-mark hold` until a matching XtraBackup is out.
- **Turning GTIDs on:** none of the servers replicate yet (October 2026), so setup can switch
  GTIDs on by itself. If replication is added later, it must be set up with GTIDs on both
  sides. Setup refuses to change `gtid_mode` on a server that has replication configured
  (`SHOW REPLICA STATUS` or connected replicas), and explains why.
- **MySQL package:** the servers install the `mysql-server` package with apt, with 8.4.x as the
  candidate (Ubuntu 26.04's archive, or Oracle's MySQL apt repository on older releases).
  Depending on which, and on what was answered at install, `root` logs in through the socket
  or with a password. Setup handles both. It first tries `mysql -u root` as the OS root user
  (socket auth). If that's refused, the wizard (or `setup --mysql-root-password-stdin`) asks for
  the root password once, uses it only to create `liops@localhost`, and never stores it.
- **Recovery point below 5 minutes:** possible later with `mysqlbinlog --read-from-remote-server
  --raw --stop-never` (a continuous stream), at the cost of a long-running child process to
  supervise. Not in the first version.
- **Encryption at rest** (`--encrypt`) stays out, as for PostgreSQL (PLAN.md §1).

## 10. Status

**M1 (October 2026):** built on branch `mysql-support`. `engine: mysql` gives a server:

- `liops setup`: creates the `liops@localhost` MySQL account with a random password in
  `mysql.defaults_file`, logging in as root through the socket, or with
  `--mysql-root-password-stdin`. Re-running it is a no-op once the account works. Refuses
  MySQL older than 8.4, and a backup name another server uses (an `owners/` marker, or
  pgBackRest backups under that name).
- Nightly dumps (`mysqldump --single-transaction`, gzipped in Go, streamed) at the
  `mysql.full_schedule` / `diff_schedule` times, plus a users-and-grants file in
  `globals/`. Retention, deleted databases and labels are the shared PostgreSQL code.
- `run full|diff` explain that point-in-time backups come later. The daily summary, the
  dashboard, the Databases pages (MySQL restore commands) and `uninstall` know about MySQL.

Tested for real against Homebrew MySQL 8.4.13 + MinIO on a Mac: setup (socket root,
root with a password containing `#`, `"` and a space, wrong password, name owned by
another server), dumps of three databases (one named `shop-2`; a view, a trigger, a
procedure, blobs, Unicode), and restores. Every table checksum matched, both next to
the original and on a fresh server. That test found one bug, now fixed: `SHOW CREATE USER`
names the default role, which may not exist yet on a fresh server, so default roles are
now set after all accounts and grants (`SET DEFAULT ROLE`).

**M2 spike (October 2026)**, by hand in an Ubuntu 26.04 VM with Ubuntu's MySQL 8.4.11, before
any M2 code:
- Percona's apt repository has `percona-xtrabackup-84` for 26.04 (`resolute`, arm64 too):
  `percona-release enable pxb-84-lts release`. XtraBackup **8.4.0-7 backs up MySQL 8.4.11**
  (it doesn't refuse the newer 8.4 server). Streamed `--compress=zstd` full and
  `--incremental-lsn` backups work; `--extra-lsndir` gives `to_lsn` and the binlog file,
  position and GTID set.
- GTIDs turn on online with `SET GLOBAL` steps, no restart, in seconds.
- Full chain proven: full + incremental prepared, swapped in for `/var/lib/mysql`, then
  binlogs replayed from the incremental's position with `--stop-datetime`: every row up to
  the target time, nothing after (the `DROP TABLE` was skipped). Replaying the same binlogs
  again changed nothing (GTIDs skip what's applied).
- Two findings now in this plan: GTID settings must go in a `mysql.conf.d` file, not only
  `SET PERSIST` (§4), and restores on Ubuntu go in place because of AppArmor (§3.5).

**M2 step 1, setup (October 2026):** `liops setup` on MySQL now also checks XtraBackup (installed,
same series as the server), binary logging (on, ROW), GTIDs and binlog expiry (≥ 7 days).
Changes it can make are listed and nothing is changed until `liops setup --enable-gtid`,
which logs in as MySQL's root, refuses a server that replicates, clears any `SET PERSIST`
values, steps `gtid_mode` up online, and writes `mysql.conf_file`
(`/etc/mysql/mysql.conf.d/zz-liops.cnf`). Setup isn't complete until the settings are
right. New settings: `mysql.conf_file`, `mysql.retention_days` (30), `mysql.binlog_interval`
(5m). The Docker image and the VM test install XtraBackup; `make vm-test-mysql` passed.

**M2 step 2, full and diff (October 2026):** `liops run full|diff` and the nightly schedule
run XtraBackup (`app/engines/xtrabackup`), streamed to `xtrabackup/base/<UTC time>-<type>/`
as `backup.xbstream`, then `backup.json` (LSNs, binlog file and position, GTIDs, versions,
size) written last. A diff is taken against the newest complete full (`--incremental-lsn`),
or becomes a full if there is none. Dumps follow each backup, as on PostgreSQL. Storage
gained `Read`. In the VM test: a full (4.7 MB, 4 s) and a diff (170 KB) of the test data,
with XtraBackup reading liops's login from the defaults file's `[client]` group.

**M2 step 3, binlog shipping (October 2026):** the `binlogs` job (`liops run binlogs`, and
every `mysql.binlog_interval` from the scheduler) runs under its own `binlog.lock`. If MySQL
wrote anything since the last run it does `FLUSH BINARY LOGS`, then uploads, gzipped, every
closed binlog from the oldest backup's binlog file on that isn't in storage yet
(`xtrabackup/binlog/<file>.gz`), reading it from the binlog directory and checking its size
against `SHOW BINARY LOGS`. Nothing is shipped before the first backup. A needed binlog
that is neither stored nor on the server is a gap: one email per gap, then a full backup
(trigger `automatic`). Failures email once until a run works again. Runs aren't kept in the
job history; the last good run is state meta `binlog_shipped_at`. In the VM test, a row
written after the diff was found, with `mysqlbinlog`, in the shipped binlogs.

**M2 step 4, retention and health (October 2026):** after each successful MySQL backup,
`xtrabackup.PlanRetention` (pure) and `Apply` delete fulls older than `mysql.retention_days`
except the newest one before the cutoff (so the whole window stays restorable), diffs with
their full, binlogs older than the oldest kept backup's binlog file, and failed attempts
older than a day. Manifests are deleted first. A failed cleanup emails but doesn't fail the
backup. The daily report and dashboard show the last backup, "can restore back to" (the
oldest backup's end) and "newest change saved" (the last binlog run that worked); they flag
a missed scheduled backup, failing or stale binlog shipping (over 3 intervals), and, in the
daily report, setup's checks again (XtraBackup matching MySQL after an upgrade, GTIDs still
on). The backups page lists MySQL's base backups and binlogs.

**M2 steps 5 and 6, restore (October 2026):** `liops restore-files --dir DIR [--to TIME]`
picks the newest full and diff finished by the moment and the shipped binlogs from that
backup's position on (`xtrabackup.PlanRestore`, pure; it refuses a moment before the oldest
backup and a binlog gap), streams each backup from storage into `xbstream` (commands gained
`Stdin`), decompresses and prepares it, downloads the binlogs, and prints the commands that
finish the restore, filled in: stop, move `/var/lib/mysql` aside, `--copy-back`, start, one
`mysqlbinlog --start-position … --stop-datetime …` (in the server's local time) piped into
`mysql`, then a new backup name. Runbook: [restore-mysql.md](restore-mysql.md).
The integration test (`--local`, i.e. `make vm-test-mysql`) inserts a row, notes the moment,
drops the table, ships binlogs, runs `restore-files` and then the printed commands as
printed: the table comes back with the same checksum as at the moment. **It passed.**
A `RESET BINARY LOGS AND GTIDS` in the middle (Docker and VM) takes a catch-up full, starts
a new binlog generation, and ships the row written after the reset; the VM restore that
follows is then from that new generation.
Found along the way: a restored server gets a new `server_uuid` (the backup has no
`auto.cnf`), and XtraBackup copies the open binlog, so the restored server's binlog numbers
carry on from the backup's: it must use a new backup name, and setup refuses the old one.
The test then continues under a new name. The Docker mode skips the restore (MySQL is the
container's main process, so it can't be stopped and swapped).

**M3, installer and wizard (October 2026):** `install.sh` uses PostgreSQL when
`pg_lsclusters` shows a cluster, otherwise MySQL when `mysqld` exists (it must be 8.4); for
MySQL it installs XtraBackup 8.4 from Percona's repository and runs
`liops install-system --engine mysql`, which starts a new config with `engine: mysql`. The
wizard's first step on MySQL takes the backup name, the server name and, only if root needs
one, root's password (used for that request to create liops's account, never stored). Apply
checks the server and claims the backup name, then **waits for the user** with a "Turn on
GTIDs now" button (and a root password field) if GTIDs are off, then takes the first backup
and sends the email. The schedules form (wizard and Settings) edits `mysql.*` on MySQL, with
a binlog interval instead of the WAL one. `make vm-test-mysql` now installs liops with the
real `install.sh` in a VM that only has `mysql-server`, checks the service and the wizard,
runs the integration test, then drives the wizard over HTTP (`test/wizard-mysql.sh`) from a
new config to the first full backup: **all passed**. The wizard's GTID button is covered by
the controller tests (`TestMySQLWizard`).

Not done yet:
- **The integration test** `test/integration-mysql.sh` covers setup (root by socket), dumps,
  a checksum-checked restore of every database, users and grants restored after the user
  and role were dropped, a dropped database noticed, the UI, the emails, and setup again
  once root has a password. **It passed** with `--local` in an Ubuntu 26.04 Multipass VM
  on Ubuntu's own `mysql-server` (8.4.11, root by `auth_socket`) — the way the servers
  install it — with MinIO and Mailpit on the Mac (October 2026). `make vm-test-mysql`
  does all of that in one command, from a fresh VM to cleanup. Its default Docker mode
  (`make integration-mysql`, CI job `testenv-mysql`) waits for its first CI run.
- **Retention** (dumps, and MySQL base backups and binlogs) has only been tested with unit
  tests: it needs days to pass.
