# pgtijori

**pgtijori** (*tijori* is Hindi for "safe" or "vault") is a one-stop backup tool for a
PostgreSQL server. You install one binary on the database server. It then runs two layers of
backups, shows their status in a small web UI, and emails you a daily summary and any failures.

> **Status: early development.** The configuration file and CLI skeleton are in place. Backups,
> the UI and the installer are being built. Don't use it in production yet.

## What it does

| Layer | How | What you get |
|-------|-----|--------------|
| **Point-in-time backups** | [pgBackRest](https://pgbackrest.org): weekly full, nightly differential, WAL archived at least hourly | Restore the whole cluster to any moment in the retention window (default 30 days) |
| **Per-database dumps** | A nightly `pg_dump` of every database, found automatically and streamed to storage | Downloadable files to restore or export a single database (default 7 days) |

Both layers go to S3-compatible object storage, e.g. Cloudflare R2, AWS S3, Backblaze B2
or MinIO.

Planned for the first release:

- **One-command install** and a browser-based setup wizard
- **Status UI** (on `127.0.0.1` only, reached through an SSH tunnel): health, backup history,
  storage use, and per-database dump downloads
- **Email**: a daily summary and immediate failure alerts, in plain language
- Restores stay manual and use pgBackRest and `pg_restore` directly. The UI doesn't restore
  anything.

## Requirements

- Ubuntu with systemd (developed on Ubuntu 26.04)
- PostgreSQL 13 or newer from the [PGDG repository](https://www.postgresql.org/download/linux/ubuntu/)
- An S3-compatible bucket and an SMTP account

## Configuration

pgtijori reads `/etc/pgtijori/config.yaml`. The file holds secrets, so pgtijori refuses to
load it unless it is readable by its owner only (`chmod 600`).

```bash
pgtijori config init                  # write a config file with defaults
pgtijori config validate              # list everything that's missing or wrong
```

## Development

Requires Go (see `go.mod`) and, for the test environment, Docker.

```bash
make test         # unit tests
make lint         # gofmt + go vet
make build-linux  # static linux/amd64 binary in dist/
make testenv      # start PostgreSQL 18 + pgBackRest + MinIO and run the smoke test
make testenv-down # stop it and delete its data
```

Releases are built by GitHub Actions when a `v*` tag is pushed. Each release has the binary
and `SHA256SUMS` attached.

## Security

See [SECURITY.md](SECURITY.md) for how to report a vulnerability.

## License

[Apache License 2.0](LICENSE)
