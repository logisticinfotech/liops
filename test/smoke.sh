#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Starts the test environment and checks that its parts work and can reach each other:
# PostgreSQL 18 answers, pgBackRest is installed, and MinIO answers over TLS from the
# PostgreSQL container (the path pgBackRest will use).
set -euo pipefail
cd "$(dirname "$0")"

compose() { docker compose -f compose.yaml "$@"; }

compose up -d --build --wait

echo "== PostgreSQL"
compose exec -T pg runuser -u postgres -- psql -Atc "SELECT version();"

echo "== pgBackRest"
compose exec -T pg pgbackrest version

echo "== MinIO over TLS, from the pg container"
for _ in $(seq 1 30); do
    if compose exec -T pg curl -fsk -o /dev/null https://minio:9000/minio/health/live; then
        echo "MinIO is up"
        echo "Smoke test passed."
        exit 0
    fi
    sleep 2
done
echo "MinIO did not become healthy" >&2
compose logs minio >&2
exit 1
