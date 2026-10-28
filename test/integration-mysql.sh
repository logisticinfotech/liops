#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# End-to-end test of MySQL support (docs/PLAN-mysql.md, M1):
#   setup (root through the socket) → dumps → report → restore every database from its
#   dump and compare checksums → restore users and grants → a dropped database is noticed
#   → the UI → the emails → setup again when root needs a password.
#
#   ./test/integration-mysql.sh            in the Docker test environment (CI). Builds liops
#                                          for the Docker host and copies it into the container.
#   sudo ./integration-mysql.sh --local    on this machine: a throwaway Ubuntu server or VM
#                                          with mysql-server installed (docs/development.md).
#       It installs $LIOPS_BIN as /usr/local/bin/liops, and uses the S3 server, SMTP server
#       and Mailpit API in $LIOPS_TEST_S3, $LIOPS_TEST_SMTP and $LIOPS_TEST_MAILPIT. It changes
#       MySQL (databases, users, root's password): never run it on a real server.
set -euo pipefail
cd "$(dirname "$0")"

mode=docker
if [ "${1:-}" = --local ]; then mode=local; fi
s3=${LIOPS_TEST_S3:-minio:9000}
smtp=${LIOPS_TEST_SMTP:-mailpit:1025}
mailpit=${LIOPS_TEST_MAILPIT:-http://mailpit:8025}

compose() { docker compose -f compose.yaml --profile mysql "$@"; }
if [ "$mode" = local ]; then
    my() { "$@"; }
else
    my() { compose exec -T mysql "$@"; }
fi
sql() { my mysql -N -B "$@"; }
step() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

wait_mysql() {
    for _ in $(seq 1 90); do
        if my mysqladmin ping --silent 2>/dev/null; then return 0; fi
        sleep 1
    done
    if [ "$mode" = docker ]; then compose logs mysql >&2; fi
    fail "MySQL did not start"
}

if [ "$mode" = local ]; then
    step "Install liops"
    [ "$(id -u)" = 0 ] || fail "--local runs as root (sudo)"
    install -m 755 "${LIOPS_BIN:?set LIOPS_BIN to a linux build of liops}" /usr/local/bin/liops
    wait_mysql
else
    step "Build liops"
    arch=$(docker version --format '{{.Server.Arch}}')
    (cd .. && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o test/.liops-linux .)

    step "Start the test environment"
    compose up -d --build mysql
    wait_mysql
    compose cp .liops-linux mysql:/usr/local/bin/liops
    my chmod 755 /usr/local/bin/liops
fi
sql -e "SELECT VERSION()"

step "Write the config"
my sh -c 'install -d -m 700 /etc/liops && umask 077 && cat > /etc/liops/config.yaml' <<YAML
engine: mysql
server:
  name: test-mysql
  timezone: UTC
mysql:
  backup_name: test-mysql
dumps:
  labels:
    database: app
    sql: SELECT db_name, label FROM tenants
storage:
  type: s3
  s3:
    endpoint: $s3
    region: us-east-1
    bucket: liops-test
    access_key_id: liops-test
    secret_access_key: liops-test-secret
    path_style: true
    verify_tls: true
email:
  smtp_host: ${smtp%:*}
  smtp_port: ${smtp##*:}
  tls: starttls
  username: test
  password: test
  from: liops@example.com
  to: [dev@example.com]
YAML
my liops config validate

step "Create test databases, a role and a user"
sql <<'SQL'
CREATE DATABASE app;
CREATE DATABASE tenant_abc;
CREATE DATABASE `shop-2`;
CREATE TABLE app.tenants (db_name VARCHAR(64), label VARCHAR(64));
INSERT INTO app.tenants VALUES ('tenant_abc', 'abc'), ('shop-2', 'shop two');
CREATE TABLE app.orders (id INT PRIMARY KEY AUTO_INCREMENT, note VARCHAR(100), photo BLOB);
CREATE TABLE app.audit (msg VARCHAR(100));
CREATE TRIGGER app.orders_ai AFTER INSERT ON app.orders FOR EACH ROW INSERT INTO audit VALUES (CONCAT('order ', NEW.id));
INSERT INTO app.orders (note, photo) VALUES ('first', 0x00FF10), ('naïve — ünïcode ✓', NULL), ('tab\there', 0xDEADBEEF);
CREATE VIEW app.recent AS SELECT id, note FROM app.orders ORDER BY id DESC;
CREATE PROCEDURE app.count_orders() SELECT COUNT(*) FROM app.orders;
CREATE TABLE tenant_abc.items (id INT PRIMARY KEY, name VARCHAR(50));
SET SESSION cte_max_recursion_depth = 100000;
INSERT INTO tenant_abc.items
  WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 50000)
  SELECT i, MD5(i) FROM n;
CREATE TABLE `shop-2`.t (id INT PRIMARY KEY);
INSERT INTO `shop-2`.t VALUES (42);
CREATE ROLE app_reader;
GRANT SELECT ON app.* TO app_reader;
CREATE USER 'app'@'%' IDENTIFIED BY 'app-password-1' DEFAULT ROLE app_reader;
GRANT app_reader TO 'app'@'%';
GRANT INSERT, UPDATE ON app.* TO 'app'@'%';
SQL
checksums() { sql -e "CHECKSUM TABLE $1.orders, $1.audit, $1.tenants" | awk '{print $2}'; }
app_sums=$(checksums app)
abc_sum=$(sql -e "CHECKSUM TABLE tenant_abc.items" | awk '{print $2}')

step "Setup, part 1: liops's own MySQL account (root by socket); the GTID changes are listed, not made"
out=$(my liops setup)
echo "$out"
grep -q -- "--enable-gtid" <<<"$out" || fail "setup should list the GTID changes and how to approve them"
if grep -q "Setup is complete" <<<"$out"; then fail "setup can't be complete before GTIDs are on"; fi
[ "$(sql -e 'SELECT @@gtid_mode')" = OFF ] || fail "setup changed gtid_mode without being asked"

step "Setup, part 2: --enable-gtid turns GTIDs on online and keeps them in mysql.conf.d"
out=$(my liops setup --enable-gtid)
echo "$out"
grep -q "Setup is complete" <<<"$out" || fail "setup did not complete"
[ "$(sql -e 'SELECT @@gtid_mode, @@enforce_gtid_consistency')" = "$(printf 'ON\tON')" ] || fail "GTIDs aren't on"
my grep -q "^gtid_mode = ON" /etc/mysql/mysql.conf.d/zz-liops.cnf || fail "the GTID settings weren't written to mysql.conf.d"
mode=$(my stat -c %a /etc/liops/mysql.cnf)
[ "$mode" = 600 ] || fail "/etc/liops/mysql.cnf has mode $mode, expected 600"
my liops setup | grep -q "Setup is complete" || fail "setup should be safe to run again"

step "Full backup + dumps, then a diff + dumps (XtraBackup, streamed)"
my liops run full
sql -e "INSERT INTO app.orders (note) VALUES ('after the full backup')"
app_sums=$(checksums app)
my liops run diff
my liops status
base=$(my curl -fsS --user liops-test:liops-test-secret --aws-sigv4 aws:amz:us-east-1:s3 \
    "https://$s3/liops-test?list-type=2&prefix=test-mysql/xtrabackup/base/" | grep -o '<Key>[^<]*</Key>' | sed 's/<[^>]*>//g')
echo "$base"
grep -q -- '-full/backup.json$' <<<"$base" || fail "no complete full backup in storage"
grep -q -- '-diff/backup.json$' <<<"$base" || fail "no complete diff backup in storage"
grep -q -- '-diff/backup.xbstream$' <<<"$base" || fail "the diff has no stream"

step "Binlog shipping: a change after the diff reaches storage"
sql -e "INSERT INTO app.orders (note) VALUES ('shipped in a binlog')"  # after the dumps: not in them
my liops run binlogs
s3list() {
    my curl -fsS --user liops-test:liops-test-secret --aws-sigv4 aws:amz:us-east-1:s3 \
        "https://$s3/liops-test?list-type=2&prefix=test-mysql/$1" | grep -o '<Key>[^<]*</Key>' | sed 's/<[^>]*>//g'
}
binlogs=$(s3list xtrabackup/binlog/)
echo "$binlogs"
[ -n "$binlogs" ] || fail "no binlogs were shipped"
found=no
for key in $binlogs; do
    if my sh -c "curl -fsS --user liops-test:liops-test-secret --aws-sigv4 aws:amz:us-east-1:s3 'https://$s3/liops-test/$key' \
        | gunzip | mysqlbinlog --base64-output=decode-rows -v - | grep -q 'shipped in a binlog'"; then found=yes; fi
done
[ "$found" = yes ] || fail "the change made after the diff isn't in any shipped binlog"
echo "binlog shipping OK (the change is in a shipped binlog)"
n=$(wc -w <<<"$binlogs")
my liops run binlogs
[ "$(s3list xtrabackup/binlog/ | wc -w)" = "$n" ] || fail "a run with nothing new written shouldn't ship anything"
my liops run report

step "Restore every database from its dump into a new database (fetched with liops dumps url)"
fetch() { # db → /tmp/<db>.sql.gz in the container
    local key url
    key=$(my liops dumps list "$1" | awk -v p="dumps/$1/" 'index($1, p) == 1 {print $1}' | tail -1)
    [ -n "$key" ] || fail "no dump of $1 in storage"
    [[ "$key" == *.sql.gz ]] || fail "dump key $key should end in .sql.gz"
    url=$(my liops dumps url "$key")
    my curl -fsS -o "/tmp/$1.sql.gz" "$url"
}
for db in app tenant_abc shop-2; do
    fetch "$db"
    sql -e "CREATE DATABASE \`${db}_restored\`"
    my sh -c "gunzip -c '/tmp/$db.sql.gz' | mysql '${db}_restored'"
done
[ "$(checksums app_restored)" = "$app_sums" ] || fail "app: restored checksums differ"
[ "$(sql -e 'CHECKSUM TABLE tenant_abc_restored.items' | awk '{print $2}')" = "$abc_sum" ] || fail "tenant_abc: restored checksum differs"
[ "$(sql -e "SELECT id FROM \`shop-2_restored\`.t")" = 42 ] || fail "shop-2 wasn't restored"
[ "$(sql -e "SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = 'app_restored'")" = 1 ] || fail "the trigger wasn't restored"
[ "$(sql -e "SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = 'app_restored'")" = 1 ] || fail "the procedure wasn't restored"
[ "$(sql -e 'SELECT COUNT(*) FROM app_restored.recent')" = 4 ] || fail "the view wasn't restored"
echo "dump restores OK (checksums match)"
for db in app tenant_abc shop-2; do sql -e "DROP DATABASE \`${db}_restored\`"; done

step "Restore users and grants after the user and its role are gone"
key=$(my liops dumps list | awk '$1 ~ /^globals\// {print $1}' | tail -1)
[ -n "$key" ] || fail "no users and grants in storage"
my curl -fsS -o /tmp/users.sql "$(my liops dumps url "$key")"
sql -e "DROP USER 'app'@'%'; DROP ROLE app_reader"
my sh -c 'mysql < /tmp/users.sql' || fail "the users file didn't apply"
my sh -c 'mysql < /tmp/users.sql' || fail "the users file should be safe to apply twice"
got=$(my mysql -N -B -u app -papp-password-1 -h 127.0.0.1 -e "SELECT REPLACE(CURRENT_ROLE(), CHAR(96), ''), (SELECT COUNT(*) FROM app.orders)" 2>/dev/null)
[ "$got" = "$(printf 'app_reader@%%\t5')" ] || fail "app should log in with its old password and default role, got: $got"
echo "users and grants restore OK"

step "A dropped database is noticed"
sql -e "DROP DATABASE \`shop-2\`"
my liops run dumps
out=$(my liops status)
echo "$out"
grep -q "shop-2 since" <<<"$out" || fail "shop-2 should be listed as no longer found"

step "UI: the dashboard and the backups page show the point-in-time backups"
my sh -c 'nohup liops serve >/tmp/serve.log 2>&1 &'
for _ in $(seq 1 30); do
    my curl -fsS -o /dev/null http://127.0.0.1:8899/static/app.css 2>/dev/null && break
    sleep 1
done
page=$(my curl -fsS http://127.0.0.1:8899/) || { my cat /tmp/serve.log; fail "the dashboard didn't load"; }
grep -q "Database dumps" <<<"$page" || fail "the dashboard should show the dumps"
grep -q "Can restore back to" <<<"$page" || fail "the dashboard should show how far back a restore can go"
page=$(my curl -fsS http://127.0.0.1:8899/backups) || fail "the backups page didn't load"
grep -q -- "-full</td>" <<<"$page" || fail "the backups page should list the full backup"
page=$(my curl -fsS http://127.0.0.1:8899/databases/app) || fail "the database page didn't load"
grep -q "sudo mysql" <<<"$page" || fail "the database page should show MySQL restore commands"
my pkill -f "liops serve" || true

step "Emails"
subjects=$(my curl -fsS "$mailpit/api/v1/messages" | grep -o '"Subject":"[^"]*"' || true)
echo "$subjects"
grep -q "backups healthy" <<<"$subjects" || fail "no healthy daily summary email"
grep -q "database no longer found: shop-2" <<<"$subjects" || fail "no deleted-database email"

step "Setup when MySQL's root needs a password"
sql -e "DROP USER 'liops'@'localhost'"
my rm /etc/liops/mysql.cnf
sql -e "ALTER USER 'root'@'localhost' IDENTIFIED WITH caching_sha2_password BY 'Root#pass \"w0rd'"
if out=$(my liops setup 2>&1); then fail "setup should ask for root's password"; fi
grep -q -- "--mysql-root-password-stdin" <<<"$out" || fail "setup should say how to pass root's password, got: $out"
out=$(printf '%s\n' 'Root#pass "w0rd' | my liops setup --mysql-root-password-stdin)
grep -q "Setup is complete" <<<"$out" || fail "setup with root's password did not complete"
my liops run dumps

if [ "$mode" = docker ]; then rm -f .liops-linux; fi
echo
echo "MySQL integration test passed."
