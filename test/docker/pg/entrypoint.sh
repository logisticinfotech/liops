#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs the cluster created by the postgresql-18 package (18/main) in the foreground.
set -euo pipefail

install -d -o postgres -g postgres -m 2775 /var/run/postgresql

exec runuser -u postgres -- /usr/lib/postgresql/18/bin/postgres \
    -c config_file=/etc/postgresql/18/main/postgresql.conf
