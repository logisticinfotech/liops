# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

pgtijori is a single static Go binary that runs on a PostgreSQL server (as root, under systemd)
and manages two backup layers to S3-compatible storage: pgBackRest point-in-time backups
(layer 1) and a nightly streamed `pg_dump` per database (layer 2), plus a loopback-only status
UI and SMTP emails. **`docs/PLAN.md` is the design source of truth.** It covers decisions,
the planned package layout (§8.2), and build phases (§9). Read the relevant section before
building a new feature. Phases 0–3 are implemented (engine, UI, installer, wizard); §10
"Status" in PLAN.md says what is still untested. Keep it current.

## Commands

```bash
make test           # go test -race -count=1 ./...
make lint           # gofmt check + go vet (CI fails on unformatted files)
make build          # local binary ./pgtijori
make build-linux    # CGO_ENABLED=0 static linux/amd64 into dist/
make vuln           # govulncheck
make testenv        # Docker: PostgreSQL 18 + pgBackRest + MinIO + Mailpit (TLS), runs test/smoke.sh
make integration    # Docker end-to-end: setup, backups, dumps, emails, dump + PITR restores
make testenv-down   # tear down and delete volumes
make dev-up         # laptop/macOS dev without Docker: MinIO + Mailpit + dev/config.yaml
make dev            # serve against it (PGTIJORI_DEV=1); see docs/development.md

go test -run TestUIMustListenOnLoopback ./config   # single test
```

CI (`.github/workflows/ci.yml`) runs gofmt, vet, race tests, the static build, shellcheck,
govulncheck, the Docker smoke test and `test/integration.sh`. Releases are built only by `release.yml` on a `v*` tag; version and
commit are injected via `-ldflags -X .../app/version.Version/Commit`.

## Architecture and conventions

- **Plain Go, minimal dependencies, no web framework** (PLAN.md §3). It runs as root and holds
  credentials, so justify every new dependency. The approved stack is listed in PLAN.md §8.1
  (net/http, html/template+embed, robfig/cron, minio-go, go-mail, bbolt, yaml.v3). The folder layout follows Laravel/Goravel conventions (`app/`, `config/`,
  `resources/views/`).
- **CLI** (`main.go`): hand-rolled subcommand dispatch with a `run(args, out)` function, so it's
  testable (`main_test.go`). Each subcommand uses its own `flag.FlagSet`. Keep the `usage`
  string in sync when adding commands.
- **Config** (`config/config.go`): `Parse` decodes YAML *on top of* `Default()` with
  `KnownFields(true)`, so unknown keys are errors. `Validate` collects every problem and returns
  them with `errors.Join`, using `section.field: message` wording. `Load` refuses files readable
  by group/others; `Save` writes atomically (temp file + fsync + rename) with mode 0600. When
  adding a setting: add the field with a yaml tag, a default in `Default()`, validation in
  `Validate()`, and a test.
- **Invariants enforced in code**: the UI must listen on a loopback address only
  (`validateLoopback`); schedules are cron specs interpreted in `server.timezone`; dump settings
  are only validated when `dumps.enabled`.
- **External programs** go through `command.Runner` (`app/command`). DB commands run as the
  `postgres` OS user via `runuser`. Tests use `command.Fake`, `storage.Memory` and
  `notify.Recorder` instead of real programs, S3 and SMTP (see `app/jobs/jobs_test.go`).
- **Jobs** (`app/jobs`): `Runner.Run` takes a file lock (`job.lock` next to the state file),
  so jobs from `serve` and `pgtijori run` queue rather than overlap. Each job is recorded as
  a `state.Run`. `full`/`diff` are followed by a separate `dumps` run. Failures email straight
  away, and a broken state file or a failed email never stops a backup.
- **State** (`app/state`, bbolt) opens the file per transaction, because bbolt allows one
  process at a time and both the service and the CLI use it.
- **Dumps** stream `pg_dump` stdout through an `io.Pipe` into a multipart upload; there is no
  local file, and a failed dump leaves no object behind. Retention rules are a pure function
  (`pgdump.PlanRetention`) that the job applies. Database names are hex-encoded in discovery
  and passed as `dbname='…'` (`postgres.Conn`), never bare.
- **Service and UI** (`app/service`, `app/http/{controllers,middleware}`, `resources/`):
  `serve` runs `service.Service`, which holds the live config, runner and scheduler. Settings
  saves and the wizard go through it; the scheduler only runs once the config is valid and
  `state` meta `setup_complete` is set (by `pgtijori setup` or the wizard). Templates are
  `resources/views/layout.html` + `partials/*.html` + one page each, embedded with `embed`.
  The strict CSP means no inline scripts or styles. Controller tests
  (`controllers_test.go`) drive the real handler through `httptest` with a cookie jar and
  fake runners. Use that harness for new pages.
- **No login** in the UI, by decision (PLAN.md §5.2): the SSH tunnel is the access control.
  Keep the protections that cost the user nothing: CSRF tokens, the localhost Host check,
  security headers. Don't add accounts back without asking.
- **Development mode** (`PGTIJORI_DEV=1`, `dev.go`): no root check, paths default to
  `./dev/`, and `setup --skip-pgbackrest` is allowed. It exists for working on a Mac, where
  `postgres.bin_dir` and `postgres.os_user` point at Homebrew PostgreSQL. It isn't a
  production feature: don't make server behaviour depend on it.
- **Setup** steps (`app/setup`) are shared by the CLI and the wizard. Never restart
  PostgreSQL without the user asking for it.
- Plain-language failure reasons come from `notify.LikelyReason` (substring table, first match
  wins). Add patterns there when a new kind of error shows up.

## Public-repo rules

- Every source file starts with an `// SPDX-License-Identifier: Apache-2.0` header (or `#` for
  shell/YAML/Makefile).
- No real hostnames, IPs, bucket names, credentials or email addresses anywhere, including tests
  and examples. Use `example.com` and placeholders. The fixed credentials in `test/compose.yaml`
  are test-only.
- User-facing text (errors, emails, CLI output) is plain language aimed at developers, and says
  what to do next, e.g. the `chmod 600` hint in `Load`.
