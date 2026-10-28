#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs the MySQL integration test (test/integration-mysql.sh --local) in a fresh Ubuntu VM
# on a Mac, without Docker: the VM gets Ubuntu's own mysql-server, the way the servers
# install it, then liops through scripts/install.sh (which installs XtraBackup and the
# service, and must open the wizard at the MySQL step). It talks to throwaway MinIO and
# Mailpit on the Mac. Everything is removed afterwards. See docs/development.md.
#
#   scripts/vm-test-mysql.sh            create the VM, run the test, delete the VM
#   scripts/vm-test-mysql.sh --keep     keep the VM afterwards (multipass shell liops-vmtest)
#
# Needs: brew install --cask multipass; brew install minio mailpit; Go.
#
# MinIO and Mailpit listen only on the Mac's address on the VM network, with a certificate
# made for that address. Only the VM trusts it: nothing is added to the Mac's keychain.
set -euo pipefail

cd "$(dirname "$0")/.."
VM=liops-vmtest
UBUNTU=26.04
S3_PORT=19100
SMTP_PORT=11025
MAILPIT_PORT=18025
S3_USER=liops-test
S3_SECRET=liops-test-secret
BUCKET=liops-test

die() { echo "vm-test-mysql.sh: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null || die "$1 not found. Install it: $2"; }
step() { printf '\n### %s\n' "$*"; }
vm() { multipass exec "$VM" -- "$@"; }

keep=false
case "${1:-}" in
	"") ;;
	--keep) keep=true ;;
	*) die "unknown option $1 (expected --keep)" ;;
esac

need multipass "brew install --cask multipass"
need minio "brew install minio"
need mailpit "brew install mailpit"
need go "https://go.dev/dl/"
need openssl "brew install openssl"
if multipass info "$VM" >/dev/null 2>&1; then
	die "a VM called $VM already exists (from a --keep run?). Remove it first: multipass delete --purge $VM"
fi

tmp=$(mktemp -d)
pids=()
cleanup() {
	local status=$?
	for pid in ${pids[@]+"${pids[@]}"}; do kill "$pid" 2>/dev/null || true; done
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

step "Create the VM (Ubuntu $UBUNTU) and install mysql-server"
multipass launch "$UBUNTU" --name "$VM" --cpus 2 --memory 4G --disk 16G >/dev/null
vm sudo bash -s <<-'EOF'
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq
	apt-get install -y -qq mysql-server curl >/dev/null
	mysqld --version
EOF

host=$(vm ip route | awk '/^default/ {print $3; exit}')
[ -n "$host" ] || die "couldn't find the Mac's address on the VM network"
arch=$(vm dpkg --print-architecture)
echo "The Mac is $host on the VM network; the VM is $arch."

step "Start MinIO and Mailpit on $host"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=liops vm test" \
	-addext "subjectAltName=IP:$host" -keyout "$tmp/private.key" -out "$tmp/public.crt" 2>/dev/null
mkdir -p "$tmp/certs" "$tmp/minio"
cp "$tmp/private.key" "$tmp/public.crt" "$tmp/certs/"
MINIO_ROOT_USER=$S3_USER MINIO_ROOT_PASSWORD=$S3_SECRET \
	minio server "$tmp/minio" --certs-dir "$tmp/certs" --address "$host:$S3_PORT" --console-address 127.0.0.1:19101 \
	>"$tmp/minio.log" 2>&1 &
pids+=($!)
mailpit --listen "$host:$MAILPIT_PORT" --smtp "$host:$SMTP_PORT" --database "$tmp/mailpit.db" \
	--smtp-tls-cert "$tmp/public.crt" --smtp-tls-key "$tmp/private.key" --smtp-require-starttls --smtp-auth-accept-any \
	>"$tmp/mailpit.log" 2>&1 &
pids+=($!)

multipass transfer "$tmp/public.crt" "$VM:/tmp/liops-vm-test.crt"
vm sudo bash -s <<-EOF
	cp /tmp/liops-vm-test.crt /usr/local/share/ca-certificates/
	update-ca-certificates >/dev/null 2>&1
	for _ in \$(seq 1 30); do
		curl -fsS -o /dev/null https://$host:$S3_PORT/minio/health/live 2>/dev/null && break
		sleep 1
	done
	curl -fsS -o /dev/null https://$host:$S3_PORT/minio/health/live || { echo "MinIO isn't reachable from the VM (is the macOS firewall blocking it?)" >&2; exit 1; }
	curl -fsS -o /dev/null http://$host:$MAILPIT_PORT/api/v1/messages || { echo "Mailpit isn't reachable from the VM" >&2; exit 1; }
	curl -fsS -X PUT --user $S3_USER:$S3_SECRET --aws-sigv4 aws:amz:us-east-1:s3 -o /dev/null https://$host:$S3_PORT/$BUCKET
	echo "MinIO and Mailpit are reachable from the VM, over verified TLS."
EOF

step "Build liops for linux/$arch, and install it with scripts/install.sh"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$tmp/liops" .
multipass transfer "$tmp/liops" "$VM:/home/ubuntu/liops"
multipass transfer scripts/install.sh "$VM:/home/ubuntu/install.sh"
multipass transfer test/integration-mysql.sh "$VM:/home/ubuntu/integration-mysql.sh"
vm sudo env LIOPS_BINARY=/home/ubuntu/liops bash /home/ubuntu/install.sh
vm sudo bash -s <<-'EOF'
	set -e
	fail() { echo "FAIL: $*" >&2; exit 1; }
	xtrabackup --version >/dev/null 2>&1 || fail "install.sh didn't install XtraBackup"
	systemctl is-active --quiet liops || fail "the liops service isn't running"
	grep -q '^engine: mysql$' /etc/liops/config.yaml || fail "install.sh should start the config with engine: mysql"
	for _ in $(seq 1 30); do curl -fsS -o /dev/null http://127.0.0.1:8899/static/app.css 2>/dev/null && break; sleep 1; done
	[ "$(curl -fsS -o /dev/null -w '%{redirect_url}' http://127.0.0.1:8899/setup)" = "http://127.0.0.1:8899/setup/mysql" ] ||
		fail "the wizard should start at the MySQL step"
	curl -fsS http://127.0.0.1:8899/setup/mysql | grep -q "MySQL root password" || fail "the MySQL step didn't render"
	echo "install.sh: XtraBackup installed, service running, wizard at the MySQL step."
	# The integration test drives liops from the command line; the service would race it.
	systemctl stop liops
EOF

step "Run test/integration-mysql.sh --local in the VM"
vm sudo env LIOPS_BIN=/home/ubuntu/liops \
	LIOPS_TEST_S3="$host:$S3_PORT" LIOPS_TEST_SMTP="$host:$SMTP_PORT" LIOPS_TEST_MAILPIT="http://$host:$MAILPIT_PORT" \
	bash /home/ubuntu/integration-mysql.sh --local

step "Run test/wizard-mysql.sh in the VM: the setup wizard over HTTP, against the installed service"
multipass transfer test/wizard-mysql.sh "$VM:/home/ubuntu/wizard-mysql.sh"
# Through stdin: multipass joins arguments into one command line, and the password (set
# by the integration test) has quotes in it.
vm sudo bash -s <<EOF
export LIOPS_TEST_S3="$host:$S3_PORT" LIOPS_TEST_SMTP="$host:$SMTP_PORT" LIOPS_TEST_MAILPIT="http://$host:$MAILPIT_PORT"
export MYSQL_ROOT_PASSWORD='Root#pass "w0rd'
bash /home/ubuntu/wizard-mysql.sh
EOF
