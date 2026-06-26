#!/usr/bin/env bash
#
# AUTHORITATIVE WordPress.org Plugin Check on a real WordPress, via Docker.
# Spins up mariadb + wordpress:cli, installs WP + the official plugin-check
# plugin, installs the plugin-under-test from its built zip, and runs
# `wp plugin check`. Exits non-zero on ANY error row.
#
# Usage:  PLUGIN_ZIP=/abs/path/to/<slug>.zip ./run.sh
# (driven by `make agent-plugincheck`).
#
set -euo pipefail
cd "$(dirname "$0")"

: "${PLUGIN_ZIP:?set PLUGIN_ZIP to the built plugin zip}"
[ -f "$PLUGIN_ZIP" ] || { echo "run.sh: PLUGIN_ZIP not found: $PLUGIN_ZIP" >&2; exit 2; }

# Derive the slug from the zip's top-level directory so we always check the
# real shipped identity, never a hardcoded (possibly stale) slug.
# Read the full listing into a var (no `| head`, which would SIGPIPE under
# pipefail), then take the first path component = the top-level plugin dir.
_ZIP_LISTING="$(unzip -Z1 "$PLUGIN_ZIP")"
SLUG="${_ZIP_LISTING%%/*}"
[ -n "$SLUG" ] || { echo "run.sh: could not derive slug from $PLUGIN_ZIP" >&2; exit 2; }
export PLUGIN_ZIP

cleanup() { docker compose down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

# Wipe the bind-mounted WP dir so a stale plugin from a prior run (different
# slug) can never shadow the build under test. Then recreate it writable.
rm -rf wp
mkdir -p wp && chmod -R 777 wp

docker compose up -d

# Wait for the db healthcheck.
for _ in $(seq 1 40); do
  [ "$(docker compose ps db --format '{{.Health}}' 2>/dev/null)" = "healthy" ] && break
  sleep 3
done

# WP-CLI core-download spikes RAM during extract; raise the limit for every call.
WP() { docker compose exec -T wpcli php -d memory_limit=1G /usr/local/bin/wp "$@"; }

WP core download --force
WP config create --dbname=wp --dbuser=root --dbpass=wp --dbhost=db --force
WP core install --url=http://localhost --title=pc \
  --admin_user=admin --admin_password=admin --admin_email=a@b.test --skip-email
WP plugin install plugin-check --activate
WP plugin install /tmp/plugin.zip --force

echo "==================== wp plugin check: $SLUG ===================="
OUT="$(WP plugin check "$SLUG" --format=csv 2>/dev/null || true)"
echo "$OUT"
echo "==============================================================="

# CSV rows are `line,column,type,code,message` grouped under `FILE:` headers.
# Fail on any ERROR row.
if printf '%s\n' "$OUT" | grep -q ',ERROR,'; then
  echo "PLUGIN CHECK FAILED: ERROR rows above. Fix or justify before shipping." >&2
  exit 1
fi
echo "Plugin Check: 0 errors. (Review any WARNING rows above.)"
