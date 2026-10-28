# SPDX-License-Identifier: Apache-2.0
#
# Test-only image: Ubuntu 26.04 + MySQL 8.4 from Ubuntu's own mysql-server package, the
# way the servers liops backs up install it (apt install mysql-server), and Percona
# XtraBackup 8.4 from Percona's repository. MySQL's root logs in through the socket
# (auth_socket), as on those servers.
FROM ubuntu:26.04

ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg lsb-release mysql-server \
 && curl -fsSLO https://repo.percona.com/apt/percona-release_latest.generic_all.deb \
 && apt-get install -y --no-install-recommends ./percona-release_latest.generic_all.deb \
 && rm percona-release_latest.generic_all.deb \
 && percona-release enable pxb-84-lts release \
 && apt-get install -y --no-install-recommends percona-xtrabackup-84 \
 && rm -rf /var/lib/apt/lists/* \
 && xtrabackup --version \
 && mysqld --version \
 && { mysqld --version | grep -q ' 8\.4\.' || { echo "this test needs MySQL 8.4; the package installed another version" >&2; exit 1; }; }

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 755 /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
