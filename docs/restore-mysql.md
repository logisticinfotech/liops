# Restoring MySQL from liops backups

liops takes the backups. Restores are done by hand over SSH, following this page. The UI
never restores anything, and `liops restore-files` only prepares files: replacing a live
database is a person's decision. (For PostgreSQL, see [restore.md](restore.md).)

Commands below use these placeholders. Replace them with your values from
`/etc/liops/config.yaml`:

| Placeholder | Meaning | Example |
|---|---|---|
| `shop-db-1` | the backup name (`mysql.backup_name`): the server's folder in the bucket | `shop-db-1` |
| `tenant_abc` | the database to restore | |
| `/restore/new` | an empty directory with room for the whole database (not under `/var/lib/mysql`) | |

## Which backup do I need?

| Situation | Use | Section |
|---|---|---|
| One database is damaged, or you need an old copy of one database | its nightly **dump** | [1](#1-restore-one-database-from-a-dump) |
| Someone deleted or broke data; you need everything back as it was at a certain moment | **point-in-time** backup | [2](#2-restore-the-whole-server-to-a-point-in-time) |
| The server is gone | **point-in-time** backup, on a new server | [3](#3-restore-onto-a-new-server) |
| You want to check that backups restore (do this every few months) | both, on a spare VM | [4](#4-restore-test) |

Dumps of different databases are taken minutes apart. If databases must match each other,
use a point-in-time restore.

> **Keep a copy of the storage credentials and the backup name somewhere other than the
> server**, for example in your password manager. If the server is lost, you need the
> bucket, endpoint, access key, secret key and backup name to restore anything.

## 1. Restore one database from a dump

Dumps are plain SQL from `mysqldump --single-transaction`, gzipped. Any MySQL 8.4 or newer
can load them.

**Get the dump.** Use the UI (Databases, then the database, then Download), or on the server:

```bash
sudo liops dumps list tenant_abc
sudo liops dumps url dumps/tenant_abc/2026-10-28T200000Z.sql.gz   # a link, valid 10 minutes
curl -fsSL -o /tmp/tenant_abc.sql.gz '<the link>'
```

**Load it into a new database next to the original**, check it, then swap names in your
app (or rename tables) when you're ready:

```bash
sudo mysql -e 'CREATE DATABASE `tenant_abc_restored`'
gunzip -c /tmp/tenant_abc.sql.gz | sudo mysql tenant_abc_restored
```

If MySQL's root needs a password here, use `mysql -u root -p` instead of `mysql`.

**On a new server**, load the users and grants first (`globals/<time>.sql`, the newest one:
`sudo liops dumps list` shows it), so the dump's users exist: `sudo mysql < users.sql`. It
creates accounts that don't exist, with their passwords, and applies their grants; existing
accounts are left as they are.

## 2. Restore the whole server to a point in time

This replaces **all** of the server's databases with how they were at one moment, from a
base backup (full, plus a diff) and the binlogs shipped after it. The current data is moved
aside, not deleted. Your app should be stopped while you do this.

**Pick the moment**, in `server.timezone`, just before the damage. The backups page in the UI
shows how far back you can go ("can restore back to") and when binlogs were last shipped:
changes after that can't be restored from storage.

**Prepare the files** (nothing on the server changes yet):

```bash
sudo liops restore-files --to "2026-10-28 14:05" --dir /restore/new
```

It downloads the backups and binlogs that moment needs, prepares them, and prints the
commands that finish the restore, filled in. They look like this:

```bash
# 1. Stop MySQL and liops, and keep the current data aside (nothing is deleted).
sudo systemctl stop liops mysql
sudo mv /var/lib/mysql /var/lib/mysql.before-restore

# 2. Put the backup in its place, and start MySQL.
sudo xtrabackup --copy-back --target-dir=/restore/new/full --datadir=/var/lib/mysql
sudo chown -R mysql:mysql /var/lib/mysql
sudo systemctl start mysql

# 3. Replay the changes made after the backup, up to the moment.
sudo mysqlbinlog --start-position=198 --stop-datetime='2026-10-28 14:05:00' /restore/new/binlogs/binlog.000041 ... | sudo mysql
```

Then **check the data**, and finish as in step 4 below.

**Undo one bad statement instead of stopping before it.** If you know the statement (for
example an accidental `DROP TABLE`), you can replay everything except it, so the changes
made after it survive. Find its GTID in the binlogs, after step 2 of the printed commands:

```bash
sudo mysqlbinlog --base64-output=decode-rows -v /restore/new/binlogs/* | grep -B 20 'DROP TABLE `orders`' | grep GTID_NEXT
#   SET @@SESSION.GTID_NEXT= 'a102517a-d2e9-11f1-b24f-525400707196:4711'
```

Then, in step 3, leave out `--stop-datetime` and add `--exclude-gtids='<that GTID>'`.

**4. Afterwards:**

- The restored server is a new server as far as backups go: it gets a new `server_uuid`
  (the backup doesn't include `auto.cnf`), and its binlog numbers carry on from the
  backup's, colliding with the binlogs already stored. `liops setup` refuses the old backup
  name for that reason. Give it a **new backup name**: change `mysql.backup_name` in
  `/etc/liops/config.yaml` (e.g. `shop-db-1-r2`), then
  `sudo liops setup && sudo liops run full && sudo systemctl start liops`. The old backups
  stay under the old name until you delete that folder in your storage provider's console.
- When you're sure: `sudo rm -rf /var/lib/mysql.before-restore /restore/new`.
- To go back instead: stop MySQL, move `/var/lib/mysql.before-restore` back, start MySQL.

**Why in place?** On Ubuntu, AppArmor lets `mysqld` use only its standard socket names, so a
second, restored server can't run next to the live one without changing AppArmor. To check a
restore without touching the live server, use a spare VM (section 4).

## 3. Restore onto a new server

1. Install Ubuntu, MySQL 8.4 (`sudo apt install mysql-server`) and XtraBackup 8.4 (the
   commands are in `liops setup`'s message when it's missing, or see
   [PLAN-mysql.md](PLAN-mysql.md) §10). Install the liops binary.
2. Write `/etc/liops/config.yaml` with `engine: mysql`, the **old** `mysql.backup_name` and the
   storage settings (mode 0600). Don't run `liops setup` yet: it would refuse the old name,
   which belongs to the old server.
3. Follow section 2 from "Prepare the files". `restore-files` only needs the config and the
   bucket. If the moment is "as late as possible", leave out `--to`.
4. Then section 2, step 4: a new backup name, `liops setup`, a full backup. The restored
   data includes the old server's `liops` MySQL account; setup gives it a new password.

## 4. Restore test

Every few months, restore the latest backup on a **spare VM** (section 3 with a throwaway
server), check that your app's data is there, and delete the VM. Use the same storage
credentials, but never run `liops setup` or start liops there: the VM must only read the
old server's backups.
