#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Drives the setup wizard of an installed liops on a MySQL server over HTTP, as a
# browser would (cookies, CSRF tokens): MySQL step (with root's password), storage
# (tested for real), email, schedules, labels skipped, review, apply. Then checks that
# setup is complete and the first full backup is in storage.
#
#   sudo ./wizard-mysql.sh      on a throwaway server where install.sh has run, MySQL's
#                               GTIDs are on, and root has the password in $MYSQL_ROOT_PASSWORD.
# It uses the S3, SMTP and Mailpit in $LIOPS_TEST_S3, $LIOPS_TEST_SMTP and
# $LIOPS_TEST_MAILPIT, like integration-mysql.sh --local. scripts/vm-test-mysql.sh runs it.
set -euo pipefail

ui=http://127.0.0.1:8899
jar=$(mktemp)
trap 'rm -f "$jar"' EXIT
s3=${LIOPS_TEST_S3:?}
smtp=${LIOPS_TEST_SMTP:?}
step() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

get() { curl -fsS -b "$jar" -c "$jar" "$ui$1"; }
# post PAGE PATH FIELD=VALUE...: submit a form from PAGE (for its CSRF token) and print
# where the browser would be sent next.
post() {
    local page=$1 path=$2 csrf
    shift 2
    csrf=$(get "$page" | grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="//; s/"$//')
    [ -n "$csrf" ] || fail "no CSRF token on $page"
    local args=(--data-urlencode "csrf=$csrf")
    for kv in "$@"; do args+=(--data-urlencode "$kv"); done
    curl -sS -b "$jar" -c "$jar" -o /dev/null -w '%{redirect_url}' -H "Origin: $ui" "${args[@]}" "$ui$path"
}
expect() { # expect WHERE-WE-WENT WHERE-WE-SHOULD-GO WHAT
    [ "$1" = "$ui$2" ] || fail "$3: went to '$1', expected $2"
}

step "Start from scratch: a new config for MySQL, no state"
systemctl stop liops
rm -f /etc/liops/config.yaml /etc/liops/mysql.cnf /var/lib/liops/state.db
liops install-system --engine mysql >/dev/null
for _ in $(seq 1 30); do get /static/app.css >/dev/null 2>&1 && break; sleep 1; done

step "MySQL: a backup name, and root's password (root has one on this server)"
expect "$(post /setup/mysql /setup/mysql backup_name=wizard-mysql name=wizard-test root_password="${MYSQL_ROOT_PASSWORD:?}")" /setup/storage "the MySQL step"
[ -s /etc/liops/mysql.cnf ] || fail "the MySQL step should create liops's MySQL account"

step "Storage (tested before it moves on), email, schedules, labels"
expect "$(post /setup/storage /setup/storage endpoint="$s3" region=us-east-1 bucket=liops-test \
    access_key_id=liops-test secret_access_key=liops-test-secret path_style=on verify_tls=on)" /setup/email "the storage step"
expect "$(post /setup/email /setup/email smtp_host="${smtp%:*}" smtp_port="${smtp##*:}" tls=starttls \
    username=test password=test from=liops@example.com to=dev@example.com action=next)" /setup/schedules "the email step"
expect "$(post /setup/schedules /setup/schedules name=wizard-test timezone=UTC \
    full_days=0 full_time=01:30 diff_days=1-6 diff_time=01:30 report_time=07:30 report_days='*' \
    retention_days=30 binlog_interval=5m enabled=on dump_retention_days=7 deleted_retention_days=14 concurrency=1 exclude=)" \
    /setup/labels "the schedules step"
expect "$(post /setup/labels /setup/labels action=skip)" /setup/review "the labels step"
review=$(get /setup/review)
grep -q "wizard-mysql" <<<"$review" || fail "the review should show the backup name"

step "Apply: GTIDs are already on, so it goes straight to the first backup"
post /setup/review /setup/apply/start >/dev/null
for _ in $(seq 1 180); do
    page=$(get /setup/apply/progress)
    if grep -q "Open the dashboard" <<<"$page"; then break; fi
    if grep -q 'badge failed\|Retry' <<<"$page"; then echo "$page" | sed 's/<[^>]*>//g' | grep -v '^\s*$' >&2; fail "apply failed"; fi
    sleep 2
done
grep -q "Open the dashboard" <<<"$page" || fail "apply didn't finish"
status=$(liops status)
grep -Eq '^ +full +ok' <<<"$status" || fail "the first full backup should have worked: $status"
dash=$(get /)
grep -q "Last full backup" <<<"$dash" || fail "the dashboard should open after setup"
echo "The wizard set up a MySQL server, from a new config to its first full backup."
