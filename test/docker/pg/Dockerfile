# SPDX-License-Identifier: Apache-2.0
#
# Test-only image: Ubuntu 26.04 + PostgreSQL 18 + pgBackRest from the PGDG repository,
# the same stack pgtijori is installed on in production.
FROM ubuntu:26.04

ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg lsb-release locales postgresql-common \
 && locale-gen en_US.UTF-8 \
 && /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y \
 && apt-get install -y --no-install-recommends postgresql-18 pgbackrest \
 && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 755 /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
