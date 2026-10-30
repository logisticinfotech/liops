# liops

**liops** is a one-stop backup tool for a PostgreSQL or MySQL 8.4 server (lightweight
monitoring is planned). You install one binary on the database server. It then runs two layers of
backups, shows their status in a small web UI, and emails you a daily summary and any failures.

> **Status: release candidate, not yet tested on a real server.** Every planned v1 feature
> is built: the backup engine, the UI, the installer and the setup wizard. Try it on a test
> server first; don't rely on it in production until it has run there for a while.

## What it does

| Layer | PostgreSQL | MySQL 8.4 | What you get |
|-------|-----------|-----------|--------------|
| **Point-in-time backups** | [pgBackRest](https://pgbackrest.org): weekly full, nightly differential, WAL archived at least hourly | [Percona XtraBackup](https://www.percona.com/mysql/software/percona-xtrabackup): weekly full, nightly differential, binlogs shipped every 5 minutes | Restore the whole server to any moment in the retention window (default 30 days) |
| **Per-database dumps** | A nightly `pg_dump` of every database | A nightly `mysqldump` of every database, plus users and grants | Downloadable files to restore or export a single database (default 7 days) |

Everything is streamed straight to storage, with no copy on the server's disk.

Both layers go to S3-compatible object storage, e.g. Cloudflare R2, AWS S3, Backblaze B2
or MinIO.

Also:

- **One-command install** and a browser-based setup wizard
- **Status UI** (on `127.0.0.1` only, reached through an SSH tunnel): health, backup history,
  storage use, per-database dump downloads, settings, and an audit log. There's no login:
  the SSH tunnel is the access control.
- **Email**: a daily summary and immediate failure alerts, in plain language
- Restores stay manual, following [docs/restore.md](docs/restore.md) (PostgreSQL) or
  [docs/restore-mysql.md](docs/restore-mysql.md) (MySQL; `liops restore-files` prepares the
  files and prints the commands). The UI doesn't restore anything.

## Install

On the database server (Ubuntu, with PostgreSQL 13+ from PGDG, or MySQL 8.4):

```bash
curl -fsSL https://github.com/logisticinfotech/liops/releases/latest/download/install.sh | sudo bash
```

The installer finds the database, installs pgBackRest (PostgreSQL) or Percona XtraBackup
(MySQL) if needed, checks the binary against `SHA256SUMS`, and starts the `liops` service. Then, on your own computer:

```bash
ssh -L 8899:127.0.0.1:8899 <you>@<server>
# open http://localhost:8899 and follow the setup wizard
```

The wizard asks before it changes your database server. PostgreSQL needs one restart to turn
on WAL archiving (a few seconds of downtime). MySQL needs GTIDs on, which liops turns on
online, without a restart, once you approve it. Running the installer again upgrades liops and keeps the
config. A server running **pgtijori** (liops's old name) is moved over by the same command:
its config, job history and logs are kept; then run `sudo liops setup` once. `sudo liops uninstall` removes it and leaves the backups in storage.

## Design

The design, decisions and build phases are in [docs/PLAN.md](docs/PLAN.md).

## Requirements

- Ubuntu with systemd (developed on Ubuntu 26.04)
- PostgreSQL 13 or newer from the [PGDG repository](https://www.postgresql.org/download/linux/ubuntu/),
  or MySQL 8.4 (`apt install mysql-server`; not replicating, or with GTIDs already on)
- An S3-compatible bucket and an SMTP account

## Command line

The wizard and the Settings page write `/etc/liops/config.yaml`. The file holds secrets,
so liops refuses to load it unless only its owner can read it (`chmod 600`). Over SSH:

```bash
sudo liops status                  # recent job runs
sudo liops run full                # run a job now: full, diff, dumps or report
sudo liops logs -f                 # follow the log
sudo liops dumps list [DB]         # stored dumps; `dumps url KEY` prints a download link
sudo liops config validate         # check the config after editing it by hand
sudo liops setup                   # redo the pgBackRest/PostgreSQL settings without the wizard
```

`liops help` lists everything.

## Development

Requires Go (see `go.mod`) and, for the test environment, Docker.

```bash
make test         # unit tests
make lint         # gofmt + go vet
make build-linux  # static linux/amd64 binary in dist/
make testenv      # start PostgreSQL 18 + pgBackRest + MinIO + Mailpit and run the smoke test
make integration  # setup, backups, dumps, emails and restores in that environment
make testenv-down # stop it and delete its data
```

To run everything, including the wizard and pgBackRest backups, on a Mac with Homebrew
PostgreSQL and pgBackRest (no Docker), see [docs/development.md](docs/development.md):
`make dev-up`, then `make dev`.

Releases are built by GitHub Actions when a `v*` tag is pushed. Each release has the binary
and `SHA256SUMS` attached.

## Security

See [SECURITY.md](SECURITY.md) for how to report a vulnerability.

## License

[Apache License 2.0](LICENSE)
