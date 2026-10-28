# SPDX-License-Identifier: Apache-2.0
#
# Test-only image: Ubuntu 26.04 + MySQL 8.4 from Ubuntu's own mysql-server package, the
# way the servers liops backs up install it (apt install mysql-server). MySQL's root logs
# in through the socket (auth_socket), as on those servers.
FROM ubuntu:26.04

ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl mysql-server \
 && rm -rf /var/lib/apt/lists/* \
 && mysqld --version \
 && { mysqld --version | grep -q ' 8\.4\.' || { echo "this test needs MySQL 8.4; the package installed another version" >&2; exit 1; }; }

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 755 /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
