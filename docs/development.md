# Developing pgtijori on a laptop

pgtijori targets Ubuntu servers. For quick iteration you can also run it on a laptop,
including a Mac with Homebrew PostgreSQL, without Docker. This covers **the UI, the
scheduler, dumps and emails**. It doesn't cover pgBackRest, `setup` or the wizard, which
need Linux. Use the Docker test environment for those (`make testenv`, `make integration`).
On a Mac, OrbStack or Colima can run it.

| Works on the laptop | Needs Linux (Docker) |
|---|---|
| `serve` and every UI page | `setup`, the setup wizard |
| `run dumps`, `run report`, emails | `run full`, `run diff`, WAL archiving |
| `dumps list`, `dumps url`, `status`, `logs` | point-in-time restores |
| restoring a dump with `pg_restore` | `install-system`, `uninstall` |

## One-time setup (macOS)

```bash
brew install postgresql@18 minio mailpit
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
3. Writes `dev/config.yaml`, pointing at your Homebrew PostgreSQL, MinIO and Mailpit.
4. Runs `setup --skip-pgbackrest`, which marks setup complete without touching pgBackRest,
   so the UI shows the dashboard instead of the wizard.

For another PostgreSQL version: `PG_VERSION=17 make dev-up`. Everything lives in `dev/`,
which git ignores. Delete it to start over.

## Day to day

```bash
make dev                                  # build, then serve on http://127.0.0.1:8899
PGTIJORI_DEV=1 ./pgtijori run dumps       # dump every database to MinIO now
PGTIJORI_DEV=1 ./pgtijori run report      # daily email, lands in Mailpit
PGTIJORI_DEV=1 ./pgtijori dumps list
make dev-down                             # stop MinIO and Mailpit
```

`PGTIJORI_DEV=1` is development mode:

- Commands don't need root.
- `--config`, `--state` and `--log` default to `dev/config.yaml`, `dev/state.db` and
  `dev/pgtijori.log`.
- `setup --skip-pgbackrest` is allowed. It's refused without `PGTIJORI_DEV=1`.

Never set it on a server.

The scheduler still runs the nightly full and diff backups. Without pgBackRest they fail,
and the failure email lands in Mailpit. That's expected here.

## What makes it work on a Mac

Two config settings, both written by `dev.sh`:

- `postgres.bin_dir: /opt/homebrew/opt/postgresql@18/bin`. Without it, pgtijori looks for
  `psql` and `pg_dump` in the Debian location, `/usr/lib/postgresql/<version>/bin`.
- `postgres.os_user: <your login name>`. Homebrew's PostgreSQL runs as you, not as
  `postgres`. pgtijori only switches user (with Linux's `runuser`) when `os_user` isn't the
  current user, so it never needs `runuser` here.

## Checking a dump restore

See [restore.md §5](restore.md#5-check-a-dump-on-your-laptop). It works the same with a
dump from production as with one from `make dev-up`.
