#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Trusts the test environment's self-signed certificate (MinIO and Mailpit use it), so
# liops can keep TLS verification on. Then runs the MySQL server created by the
# mysql-server package in the foreground.
set -euo pipefail

if [ -f /certs/public.crt ]; then
    cp /certs/public.crt /usr/local/share/ca-certificates/liops-test.crt
    update-ca-certificates >/dev/null
fi

# systemd creates this on a server; here nothing does.
install -d -o mysql -g mysql -m 755 /var/run/mysqld

# The package normally initialises the data directory; do it if it didn't.
if [ ! -d /var/lib/mysql/mysql ]; then
    mysqld --initialize-insecure --user=mysql
fi

exec mysqld --user=mysql
