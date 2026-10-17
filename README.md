# pgtijori

**pgtijori** (*tijori* is Hindi for "safe" or "vault") is a one-stop backup tool for a
PostgreSQL server. You install one binary on the database server. It then runs two layers of
backups, shows their status in a small web UI, and emails you a daily summary and any failures.

> **Status: release candidate, not yet tested on a real server.** Every planned v1 feature
> is built: the backup engine, the UI, the installer and the setup wizard. Try it on a test
> server first; don't rely on it in production until it has run there for a while.

## What it does

| Layer | How | What you get |
|-------|-----|--------------|
| **Point-in-time backups** | [pgBackRest](https://pgbackrest.org): weekly full, nightly differential, WAL archived at least hourly | Restore the whole cluster to any moment in the retention window (default 30 days) |
| **Per-database dumps** | A nightly `pg_dump` of every database, found automatically and streamed to storage | Downloadable files to restore or export a single database (default 7 days) |

Both layers go to S3-compatible object storage, e.g. Cloudflare R2, AWS S3, Backblaze B2
or MinIO.

Also:

- **One-command install** and a browser-based setup wizard
- **Status UI** (on `127.0.0.1` only, reached through an SSH tunnel): health, backup history,
  storage use, per-database dump downloads, settings, and an audit log. There's no login:
  the SSH tunnel is the access control.
- **Email**: a daily summary and immediate failure alerts, in plain language
- Restores stay manual and use pgBackRest and `pg_restore` directly, following
  [docs/restore.md](docs/restore.md). The UI doesn't restore anything.

## Install

On the database server (Ubuntu, PostgreSQL 13+ from PGDG):

```bash
curl -fsSL https://github.com/logisticinfotech/pgtijori/releases/latest/download/install.sh | sudo bash
```

The installer installs pgBackRest if needed, checks the binary against `SHA256SUMS`, and
starts the `pgtijori` service. Then, on your own computer:

```bash
ssh -L 8899:127.0.0.1:8899 <you>@<server>
# open http://localhost:8899 and follow the setup wizard
```

The wizard asks before it restarts PostgreSQL. Turning on WAL archiving needs one restart,
a few seconds of downtime. Running the installer again upgrades pgtijori and keeps the
config. `sudo pgtijori uninstall` removes it and leaves the backups in storage.

## Design

The design, decisions and build phases are in [docs/PLAN.md](docs/PLAN.md).

## Requirements

- Ubuntu with systemd (developed on Ubuntu 26.04)
- PostgreSQL 13 or newer from the [PGDG repository](https://www.postgresql.org/download/linux/ubuntu/)
- An S3-compatible bucket and an SMTP account

## Command line

The wizard and the Settings page write `/etc/pgtijori/config.yaml`. The file holds secrets,
so pgtijori refuses to load it unless only its owner can read it (`chmod 600`). Over SSH:

```bash
sudo pgtijori status                  # recent job runs
sudo pgtijori run full                # run a job now: full, diff, dumps or report
sudo pgtijori logs -f                 # follow the log
sudo pgtijori dumps list [DB]         # stored dumps; `dumps url KEY` prints a download link
sudo pgtijori config validate         # check the config after editing it by hand
sudo pgtijori setup                   # redo the pgBackRest/PostgreSQL settings without the wizard
```

`pgtijori help` lists everything.

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

To work on the UI, dumps and emails on a laptop (a Mac with Homebrew PostgreSQL) without
Docker, see [docs/development.md](docs/development.md): `make dev-up`, then `make dev`.

Releases are built by GitHub Actions when a `v*` tag is pushed. Each release has the binary
and `SHA256SUMS` attached.

## Security

See [SECURITY.md](SECURITY.md) for how to report a vulnerability.

## License

[Apache License 2.0](LICENSE)
