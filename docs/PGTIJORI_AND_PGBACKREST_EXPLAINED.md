# Understanding pgtijori and pgBackRest: Working, Differences, and Usage

This document explains the internal architecture, how **pgBackRest** and **pgtijori** work individually and together, and a complete usage guide for both tools.

---

## 1. Quick Summary: What is the Difference?

| Characteristic | pgBackRest | pgtijori |
| :--- | :--- | :--- |
| **Role** | Low-level **Physical Backup & Archiving Engine** | High-level **Backup Manager, Orchestrator & UI** |
| **Backup Type** | Physical cluster files + WAL (Layer 1) | **Dual Layer**: Automates pgBackRest (Layer 1) + Per-DB `pg_dump` (Layer 2) |
| **Automation** | None built-in (relies on cron/scripts) | Built-in Go scheduler, job queue, concurrency locks |
| **Interface** | CLI only | Web UI (`:8899` via SSH tunnel) + Unified CLI |
| **Monitoring** | Basic console logs | Embedded KV state (`bbolt`), audit log, run history |
| **Alerts** | None built-in | Plain-language SMTP email alerts & daily summaries |
| **Individual DB Dumps** | ❌ Cannot restore or export a single DB | ✅ Streams individual `.sql.gz` dumps to S3 |

> **Analogy:**  
> **pgBackRest** is like a high-performance **car engine** (powerful, fast, raw).  
> **pgtijori** is the **entire car** — complete with automated cruise control (scheduler), dashboard (Web UI), warning lights (email alerts), and an extra safety airbag (per-database logical dumps).

---

## 2. Deep Dive: How pgBackRest Works

**pgBackRest** is an open-source, enterprise-grade physical backup and restore tool for PostgreSQL.

### Core Concepts of pgBackRest:
1. **Stanza**: A stanza defines a PostgreSQL cluster and where its backups are stored. (In your server, the stanza is `goravel-eCommerce`).
2. **Physical / Block-Level Backup**:
   - It backs up the actual database files on the disk (`/var/lib/postgresql/18/main`), not SQL queries.
   - Uses multi-processing (`--process-max=4`), page checksum verification, and fast compression (`zstd`).
3. **Continuous WAL Archiving**:
   - PostgreSQL generates 16 MB log segments called **Write-Ahead Logs (WAL)** for every transaction.
   - PostgreSQL's `archive_command` calls `pgbackrest archive-push %p` to upload every WAL segment to Cloudflare R2 / S3 as soon as it fills or after an archive timeout.
4. **Point-In-Time Recovery (PITR)**:
   - When restoring, pgBackRest downloads the base backup files and configures PostgreSQL to replay the WAL stream up to any specified second.

### pgBackRest Working Flow:
```
[PostgreSQL Database] 
       │
       ├── (Every transaction writes to WAL) ──> archive_command ('pgbackrest archive-push') ──> [S3/R2 /stanza/archive]
       │
       └── (Scheduled Backup command) ─────────> 'pgbackrest backup' ───────────────────────────> [S3/R2 /stanza/backup]
```

---

## 3. Deep Dive: How pgtijori Works

**pgtijori** is a single, static Go binary that runs as a system daemon (`systemd`) on the database server.

### What pgtijori Adds on Top of pgBackRest:

#### Layer 1 Automation (pgBackRest Management)
- Automatically creates and configures `/etc/pgbackrest/pgbackrest.conf` and PostgreSQL's `archive_mode`.
- Detects database clusters and prevents naming collisions in multi-cluster environments (`CheckOwner`).
- Schedules and triggers **Full** and **Differential** backups with concurrency file locking (`job.lock`).

#### Layer 2: Automated Per-Database Logical Dumps (`pg_dump`)
- Discovers all databases in the cluster dynamically.
- Runs `pg_dump` for each database and **streams the output directly to S3/R2 via an `io.Pipe`**.
- **Zero local disk spooling**: Large databases do not fill up server storage while dumping.
- Manages retention rules (e.g., automatically purges dumps older than 7 days from S3).

#### Web Dashboard & Status UI
- Binds strictly to loopback (`127.0.0.1:8899`) to prevent external internet exposure.
- Access is secured via an SSH tunnel (`ssh -L 8899:127.0.0.1:8899 user@server`).
- Displays backup status, repository health, storage consumption, and clickable download links for dumps.

#### Embedded State Tracking & Monitoring
- Uses an embedded **bbolt** database (`/var/lib/pgtijori/state.db`) to record backup timings, file sizes, and status history.
- If a backup fails, translates complex PostgreSQL error messages into human-readable English hints.
- Sends instant email alerts on failure and a daily summary report via SMTP.

---

## 4. Architecture: How They Work Together

```
                                  +---------------------------------------+
                                  |            pgtijori daemon            |
                                  |   (Scheduler, State DB, Web UI :8899) |
                                  +-------------------+-------------------+
                                                      |
                         +----------------------------+----------------------------+
                         |                                                         |
                         v                                                         v
             [ Layer 1: PITR Engine ]                                    [ Layer 2: Dumps Engine ]
                    pgBackRest                                                   pg_dump
                         |                                                         |
       +-----------------+-----------------+                                       |
       |                                   |                                       |
  Full/Diff Backups                 WAL Archiving                               Nightly Dumps
  (Weekly / Nightly)           (Continuous / 10m-1h)                         (Streamed via io.Pipe)
       |                                   |                                       |
       +-----------------+-----------------+                                       |
                         |                                                         |
                         +----------------------------+----------------------------+
                                                      |
                                                      v
                                        +----------------------------+
                                        |  Cloudflare R2 / AWS S3    |
                                        |    (/goravel-eCommerce)    |
                                        +----------------------------+
```

---

## 5. Usage Guide: When to Use Which Command

### When to use `pgtijori` commands:
Use `pgtijori` for **daily operations, scheduling, monitoring, and database dumps**.

| Action | Command | What It Does |
| :--- | :--- | :--- |
| **Check overall status** | `sudo pgtijori status` | Shows recent job history, durations, and health. |
| **Run Full Backup** | `sudo pgtijori run full` | Runs a pgBackRest Full backup followed by logical dumps. |
| **Run Differential Backup** | `sudo pgtijori run diff` | Runs a pgBackRest Diff backup followed by logical dumps. |
| **Run Dumps Only** | `sudo pgtijori run dumps` | Discovers all databases and streams `.sql.gz` to S3. |
| **List Logical Dumps** | `sudo pgtijori dumps list` | Lists all available `.sql.gz` database dumps. |
| **Download a Dump** | `sudo pgtijori dumps url <KEY>` | Generates a signed S3/R2 download URL for a database. |
| **Test Email Summary** | `sudo pgtijori run report` | Sends the daily backup summary email to your inbox. |
| **Follow Daemon Logs** | `sudo pgtijori logs -f` | Tails the live daemon log file. |

---

### When to use `pgbackrest` commands directly:
Use `pgbackrest` for **low-level cluster restores, Point-In-Time Recovery, and deep repository inspection**.

| Action | Command | What It Does |
| :--- | :--- | :--- |
| **Check Stanza Info** | `sudo -u postgres pgbackrest --stanza=goravel-eCommerce info` | Shows all backup sets, timestamps, and WAL ranges. |
| **Check WAL Archiving** | `sudo -u postgres pgbackrest --stanza=goravel-eCommerce check` | Verifies archiving and configuration. |
| **Restore to Latest Point** | `sudo -u postgres pgbackrest --stanza=goravel-eCommerce --delta restore` | Restores the entire cluster to the latest available second. |
| **Point-In-Time Restore (PITR)** | `sudo -u postgres pgbackrest --stanza=goravel-eCommerce --delta --type=time "--target=..." --target-action=promote restore` | Replays WAL up to the exact minute/second specified. |
| **Restore Specific Backup Set** | `sudo -u postgres pgbackrest --stanza=goravel-eCommerce --set=<LABEL> --type=immediate --target-action=promote --delta restore` | Restores an exact Full or Diff backup without extra WAL. |

---

## 6. Summary Comparison: Which Tool to Choose for Restoring?

1. **If an entire database server crashes or you made a mistake 10 minutes ago across the cluster:**
   👉 Use **pgBackRest PITR** (`pgbackrest restore --type=time ...`). It will restore the whole cluster to that exact second.

2. **If you only need to restore or download 1 single database (e.g. `umang_backup`):**
   👉 Use **pgtijori dumps** (`pgtijori dumps list` and `pg_restore` / `psql`). It will not touch or disrupt other databases on the server.
