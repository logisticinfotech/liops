#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Trusts the test environment's self-signed certificate (MinIO and Mailpit use it), so
# pgtijori and pgBackRest can keep TLS verification on. Then runs the cluster created
# by the postgresql-18 package (18/main) in the foreground.
set -euo pipefail

if [ -f /certs/public.crt ]; then
    cp /certs/public.crt /usr/local/share/ca-certificates/pgtijori-test.crt
    update-ca-certificates >/dev/null
fi

install -d -o postgres -g postgres -m 2775 /var/run/postgresql

exec runuser -u postgres -- /usr/lib/postgresql/18/bin/postgres \
    -c config_file=/etc/postgresql/18/main/postgresql.conf
