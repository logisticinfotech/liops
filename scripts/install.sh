#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Installs or upgrades liops on an Ubuntu server with PostgreSQL from PGDG:
#
#   curl -fsSL https://github.com/logisticinfotech/liops/releases/latest/download/install.sh | sudo bash
#
# To pin a version, use that release's install.sh:
#   curl -fsSL https://github.com/logisticinfotech/liops/releases/download/v1.2.0/install.sh | sudo bash
#
# Re-running it upgrades the binary and restarts the service; the config, users and
# backups are kept. It never restarts PostgreSQL.
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

"$BIN" install-system
