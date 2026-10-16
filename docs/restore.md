# Restoring from pgtijori backups

pgtijori takes the backups. You can restore **one database in place** from the UI (Databases →
Restore), or do restores by hand over SSH with `pgbackrest` and `pg_restore`, following this
page. Cluster point-in-time restore is always manual.

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
| One database is damaged; replace it with last night's dump **in place** | UI **Restore**, or dump + `pg_restore` | [0](#0-restore-one-database-in-place-from-the-ui) / [1](#1-restore-one-database-from-a-dump) |
| One database; restore next to the original and check first | its nightly **dump** | [1](#1-restore-one-database-from-a-dump) |
| Someone deleted or broke data; you need everything back as it was at a certain time | **point-in-time** backup | [2](#2-restore-the-whole-cluster-to-a-point-in-time) |
| The server is gone | **point-in-time** backup, on a new server | [3](#3-restore-onto-a-new-server) |
| You want to check that backups restore (do this every few months) | both, without touching production | [4](#4-restore-test) |

Dumps of different databases are taken minutes apart. If databases must match each other
(for example a central database plus tenant databases), use a point-in-time restore.

> **Keep a copy of the storage credentials and the stanza name somewhere other than the
> server**, for example in your password manager. If the server is lost, you need the
> bucket, endpoint, access key, secret key and stanza name to restore anything.

## 0. Restore one database in place from the UI

On the **Databases** page (or a database's dump history), press **Restore** next to the dump.
Type the **exact database name** to confirm. pgtijori then:

1. downloads that dump from storage to a temp file,
2. terminates connections to the live database,
3. **renames** it to `<name>_old` (or `<name>_old_2` if that name is taken),
4. creates the database again under the **same name**,
5. runs `pg_restore --exit-on-error` into it.

The previous database stays on the server as `<name>_old` until you drop it. **Download** still
only fetches the dump file; it does not change PostgreSQL.

If `pg_restore` fails after the rename, pgtijori drops the partial new database and tries to
rename `<name>_old` back to `<name>`. If that rollback fails, check with `psql -l` and use
**Download** + restore by hand (§1). System databases (`postgres`, `template0`, `template1`)
cannot be restored from the UI. Every attempt is in the audit log. A restore waits if a backup
or dump job is running.

## 1. Restore one database from a dump

Dumps are pg_dump custom-format files, compressed with zstd from PostgreSQL 16. You need
`pg_restore` of the same or a newer major version. Prefer this path when you want to check the
data in a **new** database name before replacing production.

**Get the dump.** Use the UI (Databases, then Download), or do it on the server:

```bash
sudo pgtijori dumps list tenant_abc
sudo pgtijori dumps url pgbackrest/backup/main/dumps/tenant_abc/2026-10-05T200102Z.dump    # prints a link, valid 10 minutes
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
