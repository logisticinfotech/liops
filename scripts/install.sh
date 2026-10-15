#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Installs or upgrades pgtijori on an Ubuntu server with PostgreSQL from PGDG:
#
#   curl -fsSL https://github.com/logisticinfotech/pgtijori/releases/latest/download/install.sh | sudo bash
#
# To pin a version, use that release's install.sh:
#   curl -fsSL https://github.com/logisticinfotech/pgtijori/releases/download/v1.2.0/install.sh | sudo bash
#
# Re-running it upgrades the binary and restarts the service; the config, users and
# backups are kept. It never restarts PostgreSQL.
#
# For testing only: PGTIJORI_BINARY=/path/to/pgtijori-linux-amd64 installs a local
# build instead of downloading one (no checksum check).
set -euo pipefail

REPO="logisticinfotech/pgtijori"
# The release workflow replaces @VERSION@ with the release's tag.
VERSION="${PGTIJORI_VERSION:-@VERSION@}"
BIN=/usr/local/bin/pgtijori

say() { printf '\033[1m==> %s\033[0m\n' "$*"; }
die() { printf '\033[31mError: %s\033[0m\n' "$*" >&2; exit 1; }

# --- Checks -------------------------------------------------------------------------

[ "$(id -u)" -eq 0 ] || die "run this as root (pipe it to: sudo bash)"

[ -r /etc/os-release ] || die "can't tell which Linux this is (/etc/os-release is missing)"
# shellcheck disable=SC1091
. /etc/os-release
if [ "${ID:-}" != "ubuntu" ]; then
    case " ${ID_LIKE:-} " in
        *" debian "*|*" ubuntu "*) echo "Warning: pgtijori is made for Ubuntu; ${PRETTY_NAME:-this system} may work but isn't tested." ;;
        *) die "pgtijori needs Ubuntu (found ${PRETTY_NAME:-unknown})" ;;
    esac
fi

[ "$(uname -m)" = "x86_64" ] || die "pgtijori is built for x86_64 (amd64) only; this machine is $(uname -m)"
[ -d /run/systemd/system ] || die "pgtijori needs systemd"
command -v pg_lsclusters >/dev/null || die "PostgreSQL isn't installed (pg_lsclusters is missing). Install PostgreSQL 13 or newer from the PGDG repository first: https://www.postgresql.org/download/linux/ubuntu/"
[ -n "$(pg_lsclusters --no-header 2>/dev/null)" ] || die "no PostgreSQL cluster found (pg_lsclusters shows none)"
command -v curl >/dev/null || die "curl is missing (apt-get install curl)"

# --- pgBackRest ---------------------------------------------------------------------

if ! command -v pgbackrest >/dev/null; then
    say "Installing pgBackRest from the PGDG repository"
    if ! grep -rqs "apt.postgresql.org" /etc/apt/sources.list /etc/apt/sources.list.d/; then
        [ -x /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh ] ||
            die "the PGDG repository isn't set up and its setup script is missing (apt-get install postgresql-common)"
        /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y
    fi
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q pgbackrest
fi
say "pgBackRest: $(pgbackrest version)"

# --- The binary ---------------------------------------------------------------------

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if [ -n "${PGTIJORI_BINARY:-}" ]; then
    say "Installing the local build $PGTIJORI_BINARY (testing only)"
    cp "$PGTIJORI_BINARY" "$tmp/pgtijori-linux-amd64"
else
    # "@""VERSION@" is split so the release's version substitution leaves it alone.
    if [ "$VERSION" = "@""VERSION@" ] || [ "$VERSION" = "latest" ]; then
        base="https://github.com/$REPO/releases/latest/download"
        VERSION=latest
    else
        base="https://github.com/$REPO/releases/download/$VERSION"
    fi
    say "Downloading pgtijori ($VERSION)"
    curl -fsSL --retry 3 -o "$tmp/pgtijori-linux-amd64" "$base/pgtijori-linux-amd64"
    curl -fsSL --retry 3 -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
    say "Checking the download against SHA256SUMS"
    (cd "$tmp" && grep ' pgtijori-linux-amd64$' SHA256SUMS | sha256sum --check --strict --quiet) ||
        die "the downloaded binary doesn't match SHA256SUMS; nothing was installed"
fi

chmod 755 "$tmp/pgtijori-linux-amd64"
"$tmp/pgtijori-linux-amd64" version >/dev/null || die "the downloaded binary doesn't run on this machine"
# Replace atomically, so a running service keeps a complete file until it restarts.
install -m 755 "$tmp/pgtijori-linux-amd64" "$BIN.new"
mv -f "$BIN.new" "$BIN"
say "Installed $("$BIN" version)"

# --- Service ------------------------------------------------------------------------

"$BIN" install-system
