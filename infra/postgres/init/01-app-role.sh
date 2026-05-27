#!/bin/sh
# WPMgr — provision the unprivileged application login role on first DB init.
#
# This runs ONCE, from the postgres image's /docker-entrypoint-initdb.d hook,
# the first time the data directory is initialized (i.e. on an empty
# postgres-data volume). It is connected as the superuser/owner that the image
# created from POSTGRES_USER (here: wpmgr).
#
# Why this exists:
#   The WPMgr API connects as `wpmgr_app`, a NOSUPERUSER NOBYPASSRLS role, so
#   that Row-Level Security is the real tenant-isolation backstop. The app
#   HARD-FAILS at boot if its DB role can bypass RLS. The backend migration
#   (run with the privileged owner DSN) does `CREATE ROLE ... IF NOT EXISTS` and
#   GRANTs table privileges to `wpmgr_app`. By creating the role here as a LOGIN
#   role *before* migrations run, the migration's role-create becomes a no-op
#   and its grants land on this login role.
#
# Idempotent: guarded by a DO block + IF NOT EXISTS so re-running is harmless.
set -eu

APP_USER="${WPMGR_DB_APP_USER:-wpmgr_app}"
APP_PASSWORD="${WPMGR_DB_APP_PASSWORD:-wpmgr-app-dev-secret}"
DB_NAME="${POSTGRES_DB:-wpmgr}"

echo "[wpmgr-init] ensuring unprivileged app role '${APP_USER}' on db '${DB_NAME}'"

# NOTE: psql `:'var'` interpolation is SUPPRESSED inside dollar-quoted ($$..$$)
# blocks, so we pass the user/password into the DO block as session GUCs set
# via set_config(), then read them back with current_setting() *inside* the
# block. The set_config() args are themselves safely interpolated by psql.
psql -v ON_ERROR_STOP=1 \
     --username "${POSTGRES_USER}" \
     --dbname "${DB_NAME}" \
     --set=app_user="${APP_USER}" \
     --set=app_password="${APP_PASSWORD}" <<'EOSQL'
SELECT set_config('wpmgr.app_user',     :'app_user',     false);
SELECT set_config('wpmgr.app_password', :'app_password', false);

DO $$
DECLARE
    v_user text := current_setting('wpmgr.app_user');
    v_pass text := current_setting('wpmgr.app_password');
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_user) THEN
        EXECUTE format(
            'CREATE ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE INHERIT',
            v_user, v_pass
        );
        RAISE NOTICE 'created role %', v_user;
    ELSE
        -- Role already present (e.g. migration ran first): make sure it can
        -- log in with the dev password and never bypasses RLS.
        EXECUTE format(
            'ALTER ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOBYPASSRLS',
            v_user, v_pass
        );
        RAISE NOTICE 'role % already existed; reconciled login/attrs', v_user;
    END IF;
END
$$;
EOSQL

# Allow the app role to connect to the database. Table/sequence privileges are
# granted later by the backend migration (run with the owner DSN).
psql -v ON_ERROR_STOP=1 \
     --username "${POSTGRES_USER}" \
     --dbname "${DB_NAME}" \
     -c "GRANT CONNECT ON DATABASE \"${DB_NAME}\" TO \"${APP_USER}\";"

echo "[wpmgr-init] app role '${APP_USER}' ready (LOGIN, NOSUPERUSER, NOBYPASSRLS)"
