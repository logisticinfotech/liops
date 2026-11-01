#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests monitoring end to end in a fresh Ubuntu VM on a Mac (docs/PLAN-monitor.md §8): a
# server like the real ones (nginx, PHP-FPM, MySQL, PostgreSQL, Redis, a small PHP app),
# set up with `liops monitor-setup --apply`, under a scripted load with known problems.
# After ~18 minutes it checks that the monitor saw each one and that `liops verdict` names
# them. Everything is removed afterwards.
#
#   scripts/vm-test-monitor.sh            create the VM, run the test, delete the VM
#   scripts/vm-test-monitor.sh --keep     keep the VM afterwards (multipass shell liops-montest)
#
# Needs: brew install --cask multipass; Go. The load's credentials are test-only.
# It takes about half an hour; a sleeping Mac pauses the VM, so keep it awake:
#   caffeinate -i make vm-test-monitor
set -euo pipefail

cd "$(dirname "$0")/.."
VM=liops-montest
UBUNTU=26.04

die() { echo "vm-test-monitor.sh: $*" >&2; exit 1; }
step() { printf '\n### %s\n' "$*"; }
vm() { multipass exec "$VM" -- "$@"; }

keep=false
case "${1:-}" in
	"") ;;
	--keep) keep=true ;;
	*) die "unknown option $1 (expected --keep)" ;;
esac
command -v multipass >/dev/null || die "multipass not found. Install it: brew install --cask multipass"
command -v go >/dev/null || die "go not found: https://go.dev/dl/"
if multipass info "$VM" >/dev/null 2>&1; then
	die "a VM called $VM already exists (from a --keep run?). Remove it first: multipass delete --purge $VM"
fi

tmp=$(mktemp -d)
cleanup() {
	local status=$?
	rm -rf "$tmp"
	if [ "$status" != 0 ] && [ "$keep" != true ]; then
		echo "The test failed. To look inside the VM afterwards, run again with --keep." >&2
	fi
	if [ "$keep" = true ]; then
		echo "Kept the VM: multipass shell $VM (remove it with: multipass delete --purge $VM)"
	else
		multipass delete --purge "$VM" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

step "Create the VM (Ubuntu $UBUNTU) with nginx, Apache, PHP-FPM, MySQL, PostgreSQL and Redis"
multipass launch "$UBUNTU" --name "$VM" --cpus 2 --memory 4G --disk 16G >/dev/null
vm sudo bash -s <<-'EOF'
	set -e
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq
	apt-get install -y -qq nginx php-fpm php-mysql mysql-server postgresql redis-server curl >/dev/null
	# Apache next to nginx, on port 8081: kept from starting until it's moved there.
	printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d && chmod +x /usr/sbin/policy-rc.d
	apt-get install -y -qq apache2 >/dev/null
	rm /usr/sbin/policy-rc.d
	sed -i 's/^Listen 80$/Listen 8081/' /etc/apache2/ports.conf
	a2dissite -q 000-default
	echo "PHP $(ls /etc/php), $(mysqld --version | cut -d' ' -f2-4), PostgreSQL $(ls /etc/postgresql), $(redis-server --version | cut -d' ' -f3)"
EOF
arch=$(vm dpkg --print-architecture)

step "Build liops for linux/$arch and install it"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$tmp/liops" .
multipass transfer "$tmp/liops" "$VM:/home/ubuntu/liops"
vm sudo bash -s <<-'EOF'
	set -e
	install -m 0755 /home/ubuntu/liops /usr/local/bin/liops
	mkdir -p /etc/liops
	printf 'server:\n  name: montest\nmonitor:\n  enabled: true\n' > /etc/liops/config.yaml
	chmod 600 /etc/liops/config.yaml
	liops install-system >/dev/null
	systemctl restart liops
	systemctl enable --now liops-monitor
EOF

step "A small PHP app with known problems"
vm sudo bash -s <<-'EOF'
	set -e
	V=$(ls /etc/php | head -1)
	mkdir -p /srv/shop/public /srv/shop/storage/logs /srv/shop/bootstrap/cache
	touch /srv/shop/artisan
	# Settings the Apps checks look at; the queue is Redis, sessions share the cache's database.
	printf 'APP_NAME=Shop\nAPP_ENV=production\nAPP_DEBUG=false\nQUEUE_CONNECTION=redis\nSESSION_DRIVER=redis\nCACHE_STORE=redis\nREDIS_CACHE_DB=0\nDB_PASSWORD=test-only\n' > /srv/shop/.env
	cat > /srv/shop/public/index.php <<'P'
	<?php
	$uri = $_SERVER['REQUEST_URI'];
	if (str_starts_with($uri, '/report')) { usleep(1200000); echo "report"; exit; }
	if (str_starts_with($uri, '/orders')) {
	    // N+1: one lookup per item.
	    $db = new mysqli('localhost', 'shop', 'test-only', 'tenant_a');
	    for ($i = 0; $i < 40; $i++) { $db->query('SELECT * FROM customers WHERE id = ' . random_int(1, 300000))->fetch_all(); }
	    echo "orders"; exit;
	}
	echo "ok";
	P
	cat > /etc/nginx/sites-available/shop <<N
	server {
	    listen 80;
	    server_name shop.example.com;
	    root /srv/shop/public;
	    access_log /var/log/nginx/shop.access.log;
	    location / { try_files \$uri /index.php?\$query_string; }
	    location ~ \.php$ { include snippets/fastcgi-php.conf; fastcgi_pass unix:/run/php/php$V-fpm.sock; }
	}
	N
	ln -sf /etc/nginx/sites-available/shop /etc/nginx/sites-enabled/shop
	rm -f /etc/nginx/sites-enabled/default
	# The same app through Apache, with a log of its own.
	cat > /etc/apache2/sites-available/shop.conf <<A
	<VirtualHost *:8081>
	    ServerName apache.example.com
	    DocumentRoot /srv/shop/public
	    CustomLog \${APACHE_LOG_DIR}/shop.access.log combined
	    <Directory /srv/shop/public>
	        Require all granted
	    </Directory>
	</VirtualHost>
	A
	a2enmod -q proxy_fcgi setenvif >/dev/null; a2enconf -q php$V-fpm >/dev/null; a2ensite -q shop >/dev/null
	systemctl restart apache2
	# A pool too small for its slow requests.
	sed -i 's/^pm = .*/pm = static/; s/^pm.max_children = .*/pm.max_children = 2/' /etc/php/$V/fpm/pool.d/www.conf
	systemctl restart php$V-fpm nginx
	mysql -e "SET SESSION cte_max_recursion_depth = 1000000; CREATE DATABASE tenant_a;
	CREATE TABLE tenant_a.customers (id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(100), email VARCHAR(100));
	INSERT INTO tenant_a.customers (name, email) WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n < 300000) SELECT CONCAT('Customer ', n), CONCAT('c', n, '@example.com') FROM c;
	CREATE USER 'shop'@'localhost' IDENTIFIED BY 'test-only'; GRANT SELECT ON tenant_a.* TO 'shop'@'localhost';"
	# Redis: full and evicting, and a queue backlog.
	redis-cli CONFIG SET maxmemory 4mb >/dev/null
	redis-cli CONFIG SET maxmemory-policy allkeys-lru >/dev/null
	for i in $(seq 1 1500); do echo "RPUSH shop_database_queues:default job$i"; done | redis-cli >/dev/null
EOF

step "liops monitor-setup: list, then apply"
vm sudo liops monitor-setup
vm sudo liops monitor-setup --apply
vm sudo bash -s <<-'EOF'
	set -e
	systemctl restart postgresql   # loads pg_stat_statements (the test's restart; liops never does it by itself)
	systemctl restart liops-monitor liops
	grep -q 'liops-access.log liops;' /etc/nginx/sites-available/shop || { echo "FAIL: the nginx site has no liops access log" >&2; exit 1; }
	grep -q 'liops-access.log liops$' /etc/apache2/sites-available/shop.conf || { echo "FAIL: the Apache site has no liops access log" >&2; exit 1; }
	test -L /etc/apache2/conf-enabled/liops-monitor.conf || { echo "FAIL: liops's Apache conf isn't enabled" >&2; exit 1; }
EOF

step "Load for 17 minutes: slow requests, an N+1 route, table scans, a chatty log, evictions"
vm sudo bash -s <<-'EOF'
	cat > /root/load.sh <<'W'
	#!/bin/bash
	end=$((SECONDS + 1020))
	slow() { while [ $SECONDS -lt $end ]; do curl -s -o /dev/null -H "Host: shop.example.com" http://127.0.0.1/report/$((RANDOM % 9)); done; }
	# /orders only in every other 5 minutes, so its N+1 query's calls rise and fall with it.
	orders() { while [ $SECONDS -lt $end ]; do if [ $(( $(date +%s) / 300 % 2 )) = 0 ]; then curl -s -o /dev/null -H "Host: shop.example.com" http://127.0.0.1/orders/$RANDOM; fi; sleep 0.3; done; }
	scans() { while [ $SECONDS -lt $end ]; do mysql tenant_a -e "SELECT * FROM customers WHERE LOWER(name) LIKE '%acme12%' OR LOWER(email) LIKE '%acme12%'" >/dev/null; sleep 1; done; }
	chatty() { while [ $SECONDS -lt $end ]; do head -c 20000 /dev/urandom | base64 >> /srv/shop/storage/logs/laravel.log; sleep 1; done; }
	web2() { while [ $SECONDS -lt $end ]; do curl -s -o /dev/null -H "Host: apache.example.com" http://127.0.0.1:8081/; sleep 1; done; }
	# A deadlock: two transactions updating the same two rows in opposite orders.
	deadlock() { sleep 120; mysql tenant_a -e "BEGIN; UPDATE customers SET name = CONCAT(name, '') WHERE id = 1; SELECT SLEEP(2); UPDATE customers SET name = CONCAT(name, '') WHERE id = 2; COMMIT;" 2>/dev/null & mysql tenant_a -e "BEGIN; UPDATE customers SET name = CONCAT(name, '') WHERE id = 2; SELECT SLEEP(2); UPDATE customers SET name = CONCAT(name, '') WHERE id = 1; COMMIT;" 2>/dev/null; wait; }
	evict() { while [ $SECONDS -lt $end ]; do for i in $(seq 1 200); do echo "SET cache:$RANDOM$i $(head -c 2000 /dev/zero | tr '\0' x)"; done | redis-cli >/dev/null; sleep 2; done; }
	slow & slow & slow & orders & scans & chatty & evict & web2 & deadlock & wait
	W
	chmod +x /root/load.sh
	setsid nohup /root/load.sh >/dev/null 2>&1 < /dev/null &
EOF
sleep 1040
# The eviction load also evicted the queue (a finding of its own): queue the jobs again.
vm sudo bash -c 'for i in $(seq 1 1500); do echo "RPUSH shop_database_queues:default job$i"; done | redis-cli >/dev/null'
sleep 30

step "Check what the monitor saw"
vm sudo bash -s <<-'EOF'
	set -e
	fail() { echo "FAIL: $*" >&2; exit 1; }
	sock=/run/liops-monitor/monitor.sock
	get() { curl -fsS --unix-socket $sock "http://monitor$1"; }
	get /v1/db | grep -q '"statements":true' || fail "no statement statistics: $(get /v1/db)"
	get '/v1/requests?range=1h' | grep -q 'GET /orders/{n}' || fail "the /orders route wasn't counted"
	get /v1/latest | grep -q '"fpm.saturated:' || fail "no PHP-FPM numbers (root's collector)"
	get /v1/latest | grep -q '"redis.queue:shop_database_queues:default":1' || fail "the Redis queue wasn't counted"
	get /v1/latest | grep -q '"opcache.' || fail "no OPcache numbers"
	get '/v1/requests?range=1h' | grep -q 'apache.example.com' || fail "Apache's requests weren't counted"
	get '/v1/db/deadlock?server=mysql' | grep -q 'tenant_a.customers' || fail "no deadlock report: $(get '/v1/db/deadlock?server=mysql' 2>&1 | head -c 300)"
	echo "The monitor sees nginx's and Apache's requests, both databases' statements, the deadlock, PHP-FPM, OPcache and the Redis queue."
EOF

step "liops verdict"
out=$(vm sudo liops verdict) || die "liops verdict failed: $out"
echo "$out"
for want in "Fix the app first" "N+1" "GET /orders/{n}" "free PHP worker" "A log grows" "jobs are waiting" "share Redis database" "evict queued jobs"; do
	grep -qF "$want" <<<"$out" || die "the verdict doesn't mention: $want"
done
echo
echo "PASS: the verdict names every problem the load created."
