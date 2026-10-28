# liops for MySQL: design and build plan

Status: **proposed** (October 2026). Nothing here is built yet. [PLAN.md](PLAN.md) stays the
source of truth for everything this document doesn't change; read it first.

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
| GTIDs | **Required** (`gtid_mode=ON`, `enforce_gtid_consistency=ON`). Replay then skips transactions the base backup already has, and a bad transaction (e.g. an accidental `DROP TABLE`) can be skipped by its GTID. Both can be turned on **without a restart**. |
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
       xtrabackup/owner.json                     ← server_uuid, to refuse a name another server uses
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
   `mysqlbinlog --start-position=<from the manifest> --stop-datetime="<target>" binlog.* | mysql`.
   To undo one bad statement instead of stopping before it, the runbook shows
   `--exclude-gtids=<gtid of the DROP>`.

Dump restore: `gunzip -c <file>.sql.gz | mysql <db>`, shown filled in on the Databases page.

## 4. MySQL settings and privileges

Checked by setup, and changed only after asking (like the PostgreSQL restart today):

| Setting | Needed | Restart? |
|---------|--------|----------|
| `log_bin` | ON (8.4 default) | yes if it's off |
| `binlog_format` | ROW (8.4 default) | no |
| `gtid_mode`, `enforce_gtid_consistency` | ON | **no**: the documented online steps (`OFF_PERMISSIVE` → `ON_PERMISSIVE` → `ON`), with `SET PERSIST` |
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
  socket: /var/run/mysqld/mysqld.sock
  data_dir: /var/lib/mysql
  defaults_file: /etc/liops/mysql.cnf   # written by setup, 0600
  bin_dir: ""                              # empty: /usr/bin
xtrabackup:
  name: shop-db-1                          # backup name = folder in the bucket
  retention_days: 30
  full_schedule: "30 1 * * 0"
  diff_schedule: "30 1 * * 1-6"
  binlog_interval: 5m
dumps: …          # unchanged; default exclude is empty for mysql
```

- `postgres:` / `pgbackrest:` are only validated when `engine: postgres`, and `mysql:` /
  `xtrabackup:` only when `engine: mysql` (like `dumps` is only validated when enabled).
- A `cfg.BackupName()` method returns `pgbackrest.stanza` or `xtrabackup.name`.
  `storage.Folder` and everything else use it. PostgreSQL config files don't change.

## 6. Code changes

| Area | Change |
|------|--------|
| `app/jobs` | `Runner` holds a layer-1 interface (`Backup`, `Check`, `Info`) and a dumper interface (`Discover`, `Labels`, `Dump`, `DumpGlobals`) instead of `*pgbackrest.Engine` / `*pgdump.Engine`. New `binlogs` job. |
| `app/engines/mysql/` | Instance: binary paths, `Command`, `Query` (`mysql -N -B` with `--defaults-extra-file`), `Setting`. The counterpart of `engines/postgres`. |
| `app/engines/xtrabackup/` | Streamed backups, manifests, binlog shipping, gap check, `PlanRetention`, info for the UI. |
| `app/engines/mysqldump/` | Discovery, dump, users and grants, implementing `jobs.Dumper`. Keys, streaming and retention come from `app/engines/dumps` (done in M0). |
| `app/setup` | `mysql.go`: detect MySQL, create the user and `mysql.cnf`, check and enable settings, `CheckOwner` by `server_uuid`. |
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
| **M1. MySQL dumps** | `engines/mysql`, `engines/mysqldump`, users and grants, MySQL settings in config, CLI `setup`, Docker test environment | Dumps run, expire, and restore in the Docker test | 1 week |
| **M2. Point-in-time** | XtraBackup full/diff streaming, manifests, binlog shipping + gap check, retention, report checks, `restore-files`, `docs/restore-mysql.md` | §7 passes in CI | 2 weeks |
| **M3. UI, wizard, installer** | Engine-aware wizard, pages and emails, `install.sh` for MySQL | A fresh Ubuntu VM with MySQL 8.4 goes from nothing to its first backup with the installer and wizard | 1 week |

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
