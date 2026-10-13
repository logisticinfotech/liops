#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# End-to-end test of Phase 1 in the Docker test environment:
#   setup (incl. the PostgreSQL restart it asks for) → full backup + dumps → report
#   → restore one database from its dump → point-in-time restore of the whole cluster
#   → a dropped database is noticed → the emails arrived.
#
# Builds pgtijori for the Docker host's architecture and copies it into the pg container.
set -euo pipefail
cd "$(dirname "$0")"

compose() { docker compose -f compose.yaml "$@"; }
pg() { compose exec -T pg "$@"; }
psql() { pg runuser -u postgres -- psql -X -v ON_ERROR_STOP=1 -At "$@"; }
step() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

wait_pg() {
    for _ in $(seq 1 60); do
        if pg pg_isready -q -h /var/run/postgresql 2>/dev/null; then return 0; fi
        sleep 1
    done
    fail "PostgreSQL did not come back"
}

step "Build pgtijori"
arch=$(docker version --format '{{.Server.Arch}}')
(cd .. && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o test/.pgtijori-linux .)

step "Start the test environment"
compose up -d --build
wait_pg
compose cp .pgtijori-linux pg:/usr/local/bin/pgtijori
pg chmod 755 /usr/local/bin/pgtijori

step "Write the config"
pg sh -c 'install -d -m 700 /etc/pgtijori && umask 077 && cat > /etc/pgtijori/config.yaml' <<'YAML'
server:
  name: test-db
  timezone: UTC
pgbackrest:
  stanza: main
dumps:
  labels:
    database: app
    sql: SELECT db_name, label FROM tenants
storage:
  type: s3
  s3:
    endpoint: minio:9000
    region: us-east-1
    bucket: pgtijori-test
    access_key_id: pgtijori-test
    secret_access_key: pgtijori-test-secret
    path_style: true
    verify_tls: true
email:
  smtp_host: mailpit
  smtp_port: 1025
  tls: starttls
  username: test
  password: test
  from: pgtijori@example.com
  to: [dev@example.com]
YAML
pg pgtijori config validate

step "Create test databases"
psql -c "CREATE DATABASE app" -c "CREATE DATABASE tenant_abc" -c "CREATE DATABASE tenant_xyz"
psql -d app -c "CREATE TABLE tenants (db_name text, label text)" \
    -c "INSERT INTO tenants VALUES ('tenant_abc', 'abc'), ('tenant_xyz', 'xyz')"
psql -d tenant_abc -c "CREATE TABLE orders AS SELECT g AS id, md5(g::text) AS note FROM generate_series(1, 50000) g"

step "Setup, part 1: settings written, restart requested"
out=$(pg pgtijori setup)
echo "$out"
grep -q "must be restarted" <<<"$out" || fail "setup should ask for a restart"

compose restart pg
wait_pg

step "Setup, part 2: stanza created and checked"
out=$(pg pgtijori setup)
echo "$out"
grep -q "Setup is complete" <<<"$out" || fail "setup did not complete"

step "Full backup + dumps"
pg pgtijori run full
pg pgtijori status
pg runuser -u postgres -- pgbackrest --stanza=main info

step "Write a row after the backup, then archive it (the report runs pgbackrest check)"
psql -d tenant_abc -c "CREATE TABLE marker AS SELECT 'after-backup'::text AS v"
pg pgtijori run report

step "Restore tenant_abc from its dump into a new database"
key=$(compose run --rm -T --entrypoint sh bucket -c \
    'mc alias set t https://minio:9000 pgtijori-test pgtijori-test-secret --insecure >/dev/null && mc ls --insecure t/pgtijori-test/dumps/tenant_abc/' \
    | awk '{print $NF}' | tail -1)
[ -n "$key" ] || fail "no dump of tenant_abc in storage"
compose run --rm -T --entrypoint sh bucket -c \
    "mc alias set t https://minio:9000 pgtijori-test pgtijori-test-secret --insecure >/dev/null && mc cat --insecure t/pgtijori-test/dumps/tenant_abc/$key" \
    > .tenant_abc.dump
compose cp .tenant_abc.dump pg:/tmp/tenant_abc.dump
pg chmod 644 /tmp/tenant_abc.dump
psql -c "CREATE DATABASE tenant_abc_restored"
pg runuser -u postgres -- pg_restore --no-owner -d tenant_abc_restored /tmp/tenant_abc.dump
rows=$(psql -d tenant_abc_restored -c "SELECT count(*) FROM orders")
[ "$rows" = 50000 ] || fail "restored dump has $rows rows, expected 50000"
echo "dump restore OK ($rows rows)"
psql -c "DROP DATABASE tenant_abc_restored"

step "Point-in-time restore of the whole cluster to a second data directory"
pg rm -rf /tmp/pitr
pg install -d -o postgres -g postgres -m 700 /tmp/pitr
pg runuser -u postgres -- pgbackrest --stanza=main --pg1-path=/tmp/pitr restore
pg runuser -u postgres -- /usr/lib/postgresql/18/bin/pg_ctl -D /tmp/pitr -w -t 120 -l /tmp/pitr.log \
    -o "-c config_file=/etc/postgresql/18/main/postgresql.conf -c data_directory=/tmp/pitr -c port=5433 -c archive_mode=off -c external_pid_file=/tmp/pitr.pid" \
    start || { pg cat /tmp/pitr.log; fail "restored cluster did not start"; }
for _ in $(seq 1 60); do
    if [ "$(psql -p 5433 -c 'SELECT pg_is_in_recovery()' 2>/dev/null)" = f ]; then break; fi
    sleep 1
done
marker=$(psql -p 5433 -d tenant_abc -c "SELECT v FROM marker")
[ "$marker" = after-backup ] || fail "point-in-time restore is missing the row written after the backup"
echo "point-in-time restore OK (found the row written after the backup)"
pg runuser -u postgres -- /usr/lib/postgresql/18/bin/pg_ctl -D /tmp/pitr -m fast stop

step "A dropped database is noticed"
psql -c "DROP DATABASE tenant_xyz"
pg pgtijori run dumps
out=$(pg pgtijori status)
echo "$out"
grep -q "tenant_xyz since" <<<"$out" || fail "tenant_xyz should be listed as no longer found"

step "Emails"
subjects=$(pg curl -fsS http://mailpit:8025/api/v1/messages | grep -o '"Subject":"[^"]*"' || true)
echo "$subjects"
grep -q "backups healthy" <<<"$subjects" || fail "no healthy daily summary email"
grep -q "database no longer found: tenant_xyz" <<<"$subjects" || fail "no deleted-database email"

rm -f .pgtijori-linux .tenant_abc.dump
echo
echo "Integration test passed."
