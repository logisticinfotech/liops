#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Installs or upgrades liops on an Ubuntu server with PostgreSQL from PGDG (it installs
# pgBackRest), or with MySQL 8.4 (it installs Percona XtraBackup 8.4):
#
#   curl -fsSL https://github.com/logisticinfotech/liops/releases/latest/download/install.sh | sudo bash
#
# To pin a version, use that release's install.sh:
#   curl -fsSL https://github.com/logisticinfotech/liops/releases/download/v1.2.0/install.sh | sudo bash
#
# Re-running it upgrades the binary and restarts the service; the config, users and
# backups are kept. It never restarts the database.
#
# For testing only: LIOPS_BINARY=/path/to/liops-linux-amd64 installs a local
# build instead of downloading one (no checksum check).
set -euo pipefail

REPO="logisticinfotech/liops"
# The release workflow replaces @VERSION@ with the release's tag.
VERSION="${LIOPS_VERSION:-@VERSION@}"
BIN=/usr/local/bin/liops

say() { printf '\033[1m==> %s\033[0m\n' "$*"; }
die() { printf '\033[31mError: %s\033[0m\n' "$*" >&2; exit 1; }

# --- Checks -------------------------------------------------------------------------

[ "$(id -u)" -eq 0 ] || die "run this as root (pipe it to: sudo bash)"

[ -r /etc/os-release ] || die "can't tell which Linux this is (/etc/os-release is missing)"
# Read os-release in a subshell: it defines VERSION (e.g. "26.04 (Resolute Raccoon)"),
# which would overwrite ours.
# shellcheck disable=SC1091
os_field() { (. /etc/os-release && eval "printf '%s' \"\${$1:-}\""); }
os_id=$(os_field ID)
os_name=$(os_field PRETTY_NAME)
if [ "$os_id" != "ubuntu" ]; then
    case " $(os_field ID_LIKE) " in
        *" debian "*|*" ubuntu "*) echo "Warning: liops is made for Ubuntu; ${os_name:-this system} may work but isn't tested." ;;
        *) die "liops needs Ubuntu (found ${os_name:-unknown})" ;;
    esac
fi

case "$(uname -m)" in
    x86_64) ;;
    aarch64) [ -n "${LIOPS_BINARY:-}" ] || die "releases are built for x86_64 (amd64) only; on arm64, build one (GOARCH=arm64) and pass it as LIOPS_BINARY" ;;
    *) die "liops is built for x86_64 (amd64) only; this machine is $(uname -m)" ;;
esac
[ -d /run/systemd/system ] || die "liops needs systemd"
command -v curl >/dev/null || die "curl is missing (apt-get install curl)"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# --- Which database -------------------------------------------------------------------

engine=""
if command -v pg_lsclusters >/dev/null && [ -n "$(pg_lsclusters --no-header 2>/dev/null)" ]; then
    engine=postgres
fi
if command -v mysqld >/dev/null; then
    if [ -n "$engine" ]; then
        echo "Warning: both PostgreSQL and MySQL are installed. liops backs up one database server per machine," \
            "and uses PostgreSQL here. For MySQL instead, set engine: mysql in /etc/liops/config.yaml before the setup wizard."
    else
        engine=mysql
    fi
fi
[ -n "$engine" ] || die "neither PostgreSQL nor MySQL was found. Install one first: PostgreSQL 13 or newer from the PGDG repository (https://www.postgresql.org/download/linux/ubuntu/), or MySQL 8.4 (apt-get install mysql-server)"
say "Database: $engine"

# --- pgBackRest (PostgreSQL) ----------------------------------------------------------

if [ "$engine" = postgres ] && ! command -v pgbackrest >/dev/null; then
    say "Installing pgBackRest from the PGDG repository"
    if ! grep -rqs "apt.postgresql.org" /etc/apt/sources.list /etc/apt/sources.list.d/; then
        [ -x /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh ] ||
            die "the PGDG repository isn't set up and its setup script is missing (apt-get install postgresql-common)"
        /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y
    fi
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q pgbackrest
fi
[ "$engine" != postgres ] || say "pgBackRest: $(pgbackrest version)"

# --- XtraBackup (MySQL) -----------------------------------------------------------------

if [ "$engine" = mysql ]; then
    mysqld_version=$(mysqld --version)
    case "$mysqld_version" in
        *" 8.4."*) ;;
        *) die "liops backs up MySQL 8.4 (the LTS release), found: $mysqld_version" ;;
    esac
    if ! command -v xtrabackup >/dev/null; then
        say "Installing Percona XtraBackup 8.4 from Percona's repository"
        DEBIAN_FRONTEND=noninteractive apt-get install -y -q gnupg lsb-release
        curl -fsSL --retry 3 -o "$tmp/percona-release.deb" https://repo.percona.com/apt/percona-release_latest.generic_all.deb
        DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$tmp/percona-release.deb"
        percona-release enable pxb-84-lts release
        DEBIAN_FRONTEND=noninteractive apt-get install -y -q percona-xtrabackup-84
    fi
    say "XtraBackup: $(xtrabackup --version 2>&1 | tail -1)"
fi

# --- The binary ---------------------------------------------------------------------

if [ -n "${LIOPS_BINARY:-}" ]; then
    say "Installing the local build $LIOPS_BINARY (testing only)"
    cp "$LIOPS_BINARY" "$tmp/liops-linux-amd64" # the name is only used inside this script
else
    # "@""VERSION@" is split so the release's version substitution leaves it alone.
    if [ "$VERSION" = "@""VERSION@" ] || [ "$VERSION" = "latest" ]; then
        base="https://github.com/$REPO/releases/latest/download"
        VERSION=latest
    else
        base="https://github.com/$REPO/releases/download/$VERSION"
    fi
    say "Downloading liops ($VERSION)"
    curl -fsSL --retry 3 -o "$tmp/liops-linux-amd64" "$base/liops-linux-amd64"
    curl -fsSL --retry 3 -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
    say "Checking the download against SHA256SUMS"
    (cd "$tmp" && grep ' liops-linux-amd64$' SHA256SUMS | sha256sum --check --strict --quiet) ||
        die "the downloaded binary doesn't match SHA256SUMS; nothing was installed"
fi

chmod 755 "$tmp/liops-linux-amd64"
"$tmp/liops-linux-amd64" version >/dev/null || die "the downloaded binary doesn't run on this machine"
# Replace atomically, so a running service keeps a complete file until it restarts.
install -m 755 "$tmp/liops-linux-amd64" "$BIN.new"
mv -f "$BIN.new" "$BIN"
say "Installed $("$BIN" version)"

# --- Service ------------------------------------------------------------------------

"$BIN" install-system --engine "$engine"
