# Developing pgtijori on a Mac

pgtijori targets Ubuntu servers. You can also run all of it on a Mac with Homebrew
PostgreSQL and pgBackRest, without Docker or a VM: the setup wizard, full and diff backups,
WAL archiving, dumps, emails, and a point-in-time restore of the Mac's own cluster. MinIO
stands in for S3 and Mailpit catches the emails.

## How close is this to a server?

The jobs, scheduler, UI, storage, emails and pgBackRest commands are the same code. Where
an Ubuntu install and a Homebrew install differ, pgtijori picks by `postgres.layout`
(`debian` on servers, `homebrew` here):

| | Server (`debian`) | Mac (`homebrew`) |
|---|---|---|
| Finding clusters | `pg_lsclusters` | `<brew prefix>/var/postgresql@*` (`PG_VERSION`, `postmaster.pid`) |
| `psql`, `pg_dump` | `/usr/lib/postgresql/<v>/bin` | `<brew prefix>/opt/postgresql@<v>/bin` |
| PostgreSQL settings | `/etc/postgresql/<v>/main/conf.d/10-pgbackrest.conf` | `<data dir>/conf.d/10-pgbackrest.conf`, plus an `include_dir = 'conf.d'` line that setup appends to `postgresql.conf` |
| pgBackRest config | `/etc/pgbackrest/pgbackrest.conf` (its default) | `<brew prefix>/etc/pgbackrest/pgbackrest.conf`, passed with `--config` |
| Restart | `sudo systemctl restart postgresql@18-main` | `brew services restart postgresql@18` |
| OS users | pgtijori runs as root, PostgreSQL as `postgres` (`runuser`) | both run as you |

So a Mac run doesn't test those rows the way a server uses them. Neither does it test
`install.sh`, `install-system`, `uninstall` or the systemd service. CI covers the Debian
side (`make integration` in Docker). The fresh-Ubuntu-VM test in PLAN.md §10 is still the
check to do before a real deploy.

## One-time setup

```bash
brew install postgresql@18 pgbackrest minio mailpit
brew services start postgresql@18
make dev-up
```

`make dev-up` runs `scripts/dev.sh up`, which:

1. Makes a TLS certificate for `localhost` in `dev/certs/`. pgtijori always uses TLS for S3
   and SMTP, so the local servers need one. On macOS it asks for your password once to trust
   its CA in your login keychain. The CA only covers `localhost` and `127.0.0.1`, and its
   private key is deleted as soon as the certificate is signed.
2. Starts MinIO (S3 at `https://localhost:9000`, console at `https://localhost:9001`) and
   Mailpit (SMTP on port 1025, inbox at http://127.0.0.1:8025) in the background, and
   creates the `pgtijori-dev` bucket.
3. Writes `dev/config.yaml` with `postgres.layout: homebrew`, pointing at MinIO and Mailpit.
   `storage.s3.verify_tls` is off, because pgBackRest checks certificates with OpenSSL and
   doesn't see the keychain.

Then set up pgBackRest the same way as on a server, with the wizard or the command line:

```bash
make dev                                  # open http://127.0.0.1:8899: the setup wizard
# or
PGTIJORI_DEV=1 ./pgtijori setup           # asks for one restart: brew services restart postgresql@18
PGTIJORI_DEV=1 ./pgtijori setup           # again, after the restart
```

Without pgBackRest installed, `dev-up` runs `setup --skip-pgbackrest` instead. That marks
setup complete so dumps, emails and the UI work, and the nightly full and diff backups fail.

For another PostgreSQL version: `PG_VERSION=17 make dev-up`. Everything of pgtijori's lives
in `dev/`, which git ignores. Delete it to start over.

## Day to day

```bash
make dev                                  # build, then serve on http://127.0.0.1:8899
PGTIJORI_DEV=1 ./pgtijori run full        # or diff, dumps, report
PGTIJORI_DEV=1 ./pgtijori status
PGTIJORI_DEV=1 ./pgtijori dumps list
make dev-down                             # stop MinIO and Mailpit
```

`PGTIJORI_DEV=1` is development mode:

- Commands don't need root.
- `--config`, `--state` and `--log` default to `dev/config.yaml`, `dev/state.db` and
  `dev/pgtijori.log`.
- `setup --skip-pgbackrest` is allowed. It's refused without `PGTIJORI_DEV=1`.

Never set it on a server.

## Practising a point-in-time restore

This follows [restore.md §4](restore.md#4-restore-test) with Homebrew's paths. Your real
cluster isn't touched:

```bash
B=$(brew --prefix)
CONF="--config=$B/etc/pgbackrest/pgbackrest.conf"
pgbackrest $CONF --stanza=dev info
pgbackrest $CONF --stanza=dev --pg1-path=/tmp/restore-test restore
$B/opt/postgresql@18/bin/pg_ctl -D /tmp/restore-test -l /tmp/restore-test.log \
    -o "-c port=5499 -c archive_mode=off" start      # archive_mode=off: never archive into the real stanza
psql -p 5499 -l
$B/opt/postgresql@18/bin/pg_ctl -D /tmp/restore-test -m fast stop && rm -rf /tmp/restore-test
```

Only restore the Mac's own backups this way. A Linux server's backup doesn't belong on a
Mac (see [restore.md §5](restore.md#5-check-a-dump-on-your-laptop)).

## Turning WAL archiving off again

Setup turned on WAL archiving in your Homebrew PostgreSQL. While MinIO is stopped, WAL
files pile up in `pg_wal`. When you're done with pgtijori for a while:

```bash
rm "$(brew --prefix)/var/postgresql@18/conf.d/10-pgbackrest.conf"
brew services restart postgresql@18
```

The `include_dir = 'conf.d'` line in `postgresql.conf` is harmless with an empty `conf.d/`.

## Checking a dump restore

See [restore.md §5](restore.md#5-check-a-dump-on-your-laptop). It works the same with a
dump from production as with one from your Mac.
