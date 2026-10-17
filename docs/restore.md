# Restoring from pgtijori backups

pgtijori takes the backups. Restores are done by hand over SSH, with `pgbackrest` and
`pg_restore`, following this page. The UI never restores anything.

Commands below use these placeholders. Replace them with your values from
`/etc/pgtijori/config.yaml`:

| Placeholder | Meaning | Example |
|---|---|---|
| `main` | pgBackRest stanza (`pgbackrest.stanza`) | `main` |
| `18` / `main` | PostgreSQL version and cluster | `postgresql@18-main` |
| `tenant_abc` | the database to restore | |

## Which backup do I need?

| Situation | Use | Section |
|---|---|---|
| One database is damaged, or you need an old copy of one database | its nightly **dump** | [1](#1-restore-one-database-from-a-dump) |
| Someone deleted or broke data; you need everything back as it was at a certain time | **point-in-time** backup | [2](#2-restore-the-whole-cluster-to-a-point-in-time) |
| The server is gone | **point-in-time** backup, on a new server | [3](#3-restore-onto-a-new-server) |
| You want to check that backups restore (do this every few months) | both, without touching production | [4](#4-restore-test) |
| You want to check a dump on your own laptop (macOS included) | its nightly **dump** | [5](#5-check-a-dump-on-your-laptop) |

Dumps of different databases are taken minutes apart. If databases must match each other
(for example a central database plus tenant databases), use a point-in-time restore.

> **Keep a copy of the storage credentials and the stanza name somewhere other than the
> server**, for example in your password manager. If the server is lost, you need the
> bucket, endpoint, access key, secret key and stanza name to restore anything.

## 1. Restore one database from a dump

Dumps are pg_dump custom-format files, compressed with zstd from PostgreSQL 16. You need
`pg_restore` of the same or a newer major version.

**Get the dump.** Use the UI (Databases, then Download), or do it on the server:

```bash
sudo pgtijori dumps list tenant_abc
sudo pgtijori dumps url dumps/tenant_abc/2026-10-05T200102Z.dump    # prints a link, valid 10 minutes
curl -fsSL -o /tmp/tenant_abc.dump '<the link>'
sudo chown postgres: /tmp/tenant_abc.dump
```

**Restore it next to the original**, check it, and only then swap:

```bash
sudo -u postgres createdb tenant_abc_restored
sudo -u postgres pg_restore --exit-on-error -d tenant_abc_restored /tmp/tenant_abc.dump

# Check the data, e.g.:
sudo -u postgres psql -d tenant_abc_restored -c '\dt+'
```

To put it in place of the original, stop the app (or this tenant), then:

```sql
-- as postgres, connected to another database (e.g. postgres)
SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname IN ('tenant_abc', 'tenant_abc_restored');
ALTER DATABASE tenant_abc RENAME TO tenant_abc_old;
ALTER DATABASE tenant_abc_restored RENAME TO tenant_abc;
```

Drop `tenant_abc_old` once you're sure. pgtijori dumps the restored database under its
name from the next night on.

**On a server where the roles don't exist** (for example a laptop), restore the roles
first, from the newest `globals/…sql` (Databases page → Roles and tablespaces, or
`pgtijori dumps list`):

```bash
sudo -u postgres psql -f globals.sql        # errors for roles that already exist are fine
```

Or skip ownership: `pg_restore --no-owner --no-privileges -d NEWDB FILE.dump`.

## 2. Restore the whole cluster to a point in time

This replaces **every database** in the cluster with its state at the target time.
Changes made after that time are lost.

1. Find the window you can restore to:

   ```bash
   sudo -u postgres pgbackrest --stanza=main info
   ```

   The oldest backup's stop time is the earliest point. The dashboard shows it as
   "Can restore back to".

2. Stop pgtijori, the apps, and PostgreSQL:

   ```bash
   sudo systemctl stop pgtijori
   sudo systemctl stop postgresql@18-main
   ```

3. Restore. `--delta` reuses files that are already correct, which is much faster than
   restoring into an empty directory. The time is in the server's local time unless you
   give an offset:

   ```bash
   sudo -u postgres pgbackrest --stanza=main --delta \
       --type=time "--target=2026-10-05 14:30:00+05:30" --target-action=promote \
       restore
   ```

   To restore to the latest moment available (after a disk failure, for example), leave
   out `--type`, `--target` and `--target-action`.

4. Start PostgreSQL and check the data:

   ```bash
   sudo systemctl start postgresql@18-main
   sudo -u postgres psql -c 'SELECT pg_is_in_recovery();'     # f once the restore is done
   ```

5. The restored cluster is on a new timeline. Take a fresh full backup, then start
   pgtijori again:

   ```bash
   sudo pgtijori run full
   sudo systemctl start pgtijori
   ```

## 3. Restore onto a new server

1. Install the **same PostgreSQL major version** from PGDG, and pgBackRest:

   ```bash
   sudo /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y
   sudo apt-get install -y postgresql-18 pgbackrest
   sudo systemctl stop postgresql@18-main
   ```

2. Write `/etc/pgbackrest/pgbackrest.conf` with the old server's storage settings (from
   your password manager). Use the same stanza name:

   ```ini
   [global]
   repo1-type=s3
   repo1-path=/pgbackrest
   repo1-s3-endpoint=<account-id>.r2.cloudflarestorage.com
   repo1-s3-bucket=<bucket>
   repo1-s3-region=auto
   repo1-s3-key=<access key id>
   repo1-s3-key-secret=<secret access key>
   repo1-s3-uri-style=path

   [main]
   pg1-path=/var/lib/postgresql/18/main
   ```

   ```bash
   sudo chown root:postgres /etc/pgbackrest/pgbackrest.conf
   sudo chmod 640 /etc/pgbackrest/pgbackrest.conf
   ```

3. Restore into the empty data directory, then start:

   ```bash
   sudo -u postgres pgbackrest --stanza=main --delta restore
   sudo systemctl start postgresql@18-main
   ```

4. Make sure the **old server can't come back** and archive into the same stanza. Then
   install pgtijori on the new server and run the setup wizard with the same storage
   settings and stanza name. Setup creates nothing new in the bucket for an existing
   stanza; it checks it and takes a full backup.

   ```bash
   curl -fsSL https://github.com/logisticinfotech/pgtijori/releases/latest/download/install.sh | sudo bash
   ```

## 4. Restore test

Do this every few months, and after changing storage settings. It restores into a second
data directory on another port, so production isn't touched. You need free disk space
about the size of the database.

```bash
sudo install -d -o postgres -g postgres -m 700 /var/lib/postgresql/restore-test
sudo -u postgres pgbackrest --stanza=main --pg1-path=/var/lib/postgresql/restore-test restore

# archive_mode=off is essential: the copy must never send WAL into the real stanza.
sudo -u postgres /usr/lib/postgresql/18/bin/pg_ctl -D /var/lib/postgresql/restore-test -w -l /tmp/restore-test.log \
    -o "-c config_file=/etc/postgresql/18/main/postgresql.conf -c data_directory=/var/lib/postgresql/restore-test \
        -c port=5499 -c archive_mode=off -c external_pid_file=/tmp/restore-test.pid" start

sudo -u postgres psql -p 5499 -l                        # all databases are there
sudo -u postgres psql -p 5499 -d tenant_abc -c '\dt+'   # spot-check data
```

Then test one dump the same way (section 1, into a scratch database) and clean up:

```bash
sudo -u postgres /usr/lib/postgresql/18/bin/pg_ctl -D /var/lib/postgresql/restore-test -m fast stop
sudo rm -rf /var/lib/postgresql/restore-test
```

Write down the date and how long it took. `test/integration.sh` runs the same steps
automatically in CI against the Docker test environment.

## 5. Check a dump on your laptop

A dump is a logical backup (SQL objects and data, not data files), so it restores on any
operating system and CPU, including a Mac with Homebrew PostgreSQL. This is a real check
of the dump, and production isn't touched.

You need `pg_restore` of the **same or a newer major version** than the server
(`brew install postgresql@18`), plus any extensions the database uses (e.g.
`brew install postgis`). Otherwise `--exit-on-error` stops at the first `CREATE EXTENSION`.

**Get a link on the server, download on the laptop.** The link works from anywhere for 10
minutes:

```bash
# on the server
sudo pgtijori dumps list tenant_abc
sudo pgtijori dumps url dumps/tenant_abc/2026-10-05T200102Z.dump

# on the laptop
curl -fsSL -o ~/Downloads/tenant_abc.dump '<the link>'
```

**Restore into a scratch database.** `--no-owner --no-privileges` skips the server's roles,
which don't exist on the laptop:

```bash
pg_restore --version                       # same or newer major version than the server
createdb tenant_abc_check
pg_restore --exit-on-error --no-owner --no-privileges -d tenant_abc_check ~/Downloads/tenant_abc.dump
```

**Compare row counts** with production. Run this on both (on the server as
`sudo -u postgres psql -d tenant_abc`, on the laptop as `psql -d tenant_abc_check`):

```sql
SELECT table_schema, table_name,
       (xpath('/row/c/text()', query_to_xml(format('SELECT count(*) AS c FROM %I.%I', table_schema, table_name), false, true, '')))[1]::text::bigint AS rows
FROM information_schema.tables
WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')
ORDER BY 1, 2;
```

Production has changed since the dump was taken, so busy tables can differ a little. A
table that is empty on the laptop but not on the server is a problem. So is a large gap.

**Clean up.** The file and the database hold production data:

```bash
dropdb tenant_abc_check
rm ~/Downloads/tenant_abc.dump
```

**Don't check point-in-time backups on a Mac.** pgBackRest restores PostgreSQL's data files,
which are only supported on the same platform. A Linux server's files on a Mac may even
start, but macOS sorts text differently from Linux (glibc), so text indexes can be silently
wrong. Use section 4 on a Linux machine, or `make integration` in Docker (OrbStack or Colima
on a Mac).
