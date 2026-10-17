#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Development environment on a laptop (a Mac with Homebrew PostgreSQL), without Docker:
# MinIO stands in for S3 and Mailpit catches every email. Everything lives in ./dev/,
# which git ignores. See docs/development.md.
#
#   scripts/dev.sh up      certificates, MinIO, Mailpit, bucket, dev/config.yaml, setup
#   scripts/dev.sh down    stop MinIO and Mailpit (data in ./dev/ is kept)
#
# Needs: brew install minio mailpit, and PostgreSQL from Homebrew (postgresql@18 by
# default; set PG_VERSION=17 etc. for another one).
#
# pgtijori always uses TLS for S3 and SMTP, so `up` makes a local certificate for
# localhost and, on macOS, asks you once to trust its CA in your login keychain. The
# CA's private key is deleted straight after signing, and the CA is name-constrained to
# localhost, so it can't be used to issue a certificate for anything else.
set -euo pipefail

cd "$(dirname "$0")/.."
DEV=dev
CERTS=$DEV/certs
PG_VERSION="${PG_VERSION:-18}"
MINIO_USER=pgtijori-dev
MINIO_SECRET=pgtijori-dev-secret
BUCKET=pgtijori-dev
CA_NAME="pgtijori dev CA (localhost only)"

die() { echo "dev.sh: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null || die "$1 not found. Install it: $2"; }

certs() {
	[ -f "$CERTS/public.crt" ] && return
	mkdir -p "$CERTS"
	chmod 700 "$DEV" "$CERTS"
	local tmp
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' RETURN

	cat >"$tmp/ca.ext" <<-EOF
		basicConstraints=critical,CA:TRUE,pathlen:0
		keyUsage=critical,keyCertSign,cRLSign
		nameConstraints=critical,permitted;DNS:localhost,permitted;IP:127.0.0.1/255.255.255.255
	EOF
	cat >"$tmp/leaf.ext" <<-EOF
		basicConstraints=critical,CA:FALSE
		keyUsage=critical,digitalSignature,keyEncipherment
		extendedKeyUsage=serverAuth
		subjectAltName=DNS:localhost,IP:127.0.0.1
	EOF

	openssl req -new -newkey rsa:2048 -nodes -subj "/CN=$CA_NAME" -keyout "$tmp/ca.key" -out "$tmp/ca.csr" 2>/dev/null
	openssl x509 -req -in "$tmp/ca.csr" -signkey "$tmp/ca.key" -days 365 -extfile "$tmp/ca.ext" -out "$CERTS/ca.crt" 2>/dev/null
	openssl req -new -newkey rsa:2048 -nodes -subj "/CN=localhost" -keyout "$CERTS/private.key" -out "$tmp/leaf.csr" 2>/dev/null
	openssl x509 -req -in "$tmp/leaf.csr" -CA "$CERTS/ca.crt" -CAkey "$tmp/ca.key" -CAcreateserial -CAserial "$tmp/ca.srl" \
		-days 365 -extfile "$tmp/leaf.ext" -out "$CERTS/public.crt" 2>/dev/null
	chmod 600 "$CERTS/private.key"
	echo "Made a localhost certificate in $CERTS/ (valid one year; the CA key is already deleted)."
}

trust() {
	[ "$(uname)" = Darwin ] || {
		echo "Not macOS: trust $CERTS/ca.crt in your system store (e.g. update-ca-certificates)."
		return
	}
	local keychain=~/Library/Keychains/login.keychain-db
	if security find-certificate -c "$CA_NAME" "$keychain" >/dev/null 2>&1; then
		# A CA from an earlier `up` (after dev/ was deleted) can't sign today's certificate.
		security find-certificate -c "$CA_NAME" -p "$keychain" | cmp -s - "$CERTS/ca.crt" && return
		echo "Replacing the old dev CA in your login keychain..."
		security delete-certificate -c "$CA_NAME" "$keychain" >/dev/null
	fi
	echo "Trusting the dev CA in your login keychain (macOS will ask for your password)..."
	security add-trusted-cert -r trustRoot -p ssl -k "$keychain" "$CERTS/ca.crt"
}

start() { # name, command...
	local name=$1 pid=$DEV/$1.pid
	shift
	if [ -f "$pid" ] && kill -0 "$(cat "$pid")" 2>/dev/null; then
		return
	fi
	nohup "$@" >"$DEV/$name.log" 2>&1 &
	echo $! >"$pid"
	echo "Started $name (log: $DEV/$name.log)."
}

wait_for() { # url
	for _ in $(seq 30); do
		curl -fsS --cacert "$CERTS/ca.crt" -o /dev/null "$1" 2>/dev/null && return
		sleep 1
	done
	die "$1 didn't come up; see the logs in $DEV/"
}

write_config() {
	local cfg=$DEV/config.yaml
	[ -f "$cfg" ] && return
	local prefix bin
	prefix=$(brew --prefix)
	bin=$prefix/opt/postgresql@$PG_VERSION/bin
	[ -x "$bin/pg_dump" ] || die "$bin/pg_dump not found. Install it: brew install postgresql@$PG_VERSION (or set PG_VERSION)"
	(
		umask 077
		cat >"$cfg" <<-EOF
			# Development config written by scripts/dev.sh. Not for a server.
			server:
			  name: dev-laptop
			postgres:
			  version: $PG_VERSION
			  data_dir: $prefix/var/postgresql@$PG_VERSION
			  os_user: $(id -un)
			  bin_dir: $bin
			pgbackrest:
			  stanza: dev
			storage:
			  s3:
			    endpoint: localhost:9000
			    bucket: $BUCKET
			    access_key_id: $MINIO_USER
			    secret_access_key: $MINIO_SECRET
			email:
			  smtp_host: localhost
			  smtp_port: 1025
			  username: dev
			  password: dev
			  from: pgtijori@example.com
			  to: [dev@example.com]
		EOF
	)
	echo "Wrote $cfg."
}

up() {
	need minio "brew install minio"
	need mailpit "brew install mailpit"
	need openssl "brew install openssl"
	certs
	trust
	mkdir -p "$DEV/minio" "$DEV/mailpit"

	MINIO_ROOT_USER=$MINIO_USER MINIO_ROOT_PASSWORD=$MINIO_SECRET \
		start minio minio server "$DEV/minio" --certs-dir "$CERTS" --address 127.0.0.1:9000 --console-address 127.0.0.1:9001
	start mailpit mailpit --listen 127.0.0.1:8025 --smtp 127.0.0.1:1025 --database "$DEV/mailpit/mailpit.db" \
		--smtp-tls-cert "$CERTS/public.crt" --smtp-tls-key "$CERTS/private.key" \
		--smtp-require-starttls --smtp-auth-accept-any

	wait_for https://localhost:9000/minio/health/live
	# pgtijori doesn't create buckets; PUT one (already existing is fine).
	curl -sS --cacert "$CERTS/ca.crt" --aws-sigv4 "aws:amz:us-east-1:s3" --user "$MINIO_USER:$MINIO_SECRET" \
		-X PUT -o /dev/null "https://localhost:9000/$BUCKET"

	write_config
	go build -o pgtijori .
	PGTIJORI_DEV=1 ./pgtijori config validate
	PGTIJORI_DEV=1 ./pgtijori setup --skip-pgbackrest

	cat <<-EOF

		Ready. MinIO console: https://localhost:9001 ($MINIO_USER / $MINIO_SECRET)
		       Mailpit:       http://127.0.0.1:8025

		  make dev                                  # build and serve the UI on http://127.0.0.1:8899
		  PGTIJORI_DEV=1 ./pgtijori run dumps       # dump every database now
		  PGTIJORI_DEV=1 ./pgtijori run report      # send the daily email to Mailpit
	EOF
}

down() {
	for name in minio mailpit; do
		local pid=$DEV/$name.pid
		if [ -f "$pid" ]; then
			kill "$(cat "$pid")" 2>/dev/null && echo "Stopped $name."
			rm -f "$pid"
		fi
	done
}

case "${1:-}" in
up) up ;;
down) down ;;
*) die "usage: scripts/dev.sh up|down" ;;
esac
