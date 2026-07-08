-- m97: GH #174 — RUM beacon-key stuck-empty fix, ack-based re-mint half.
--
-- Root cause: the beacon-key mint gate (perf.Service.UpdateConfig) only fires
-- once — "if RumEnabled && !BeaconKeySet" — and the mint+push to the agent is
-- best-effort. If that ONE plaintext push is lost (agent down/unreachable at
-- the moment RUM is first enabled), beacon_key_hash is already committed
-- (BeaconKeySet permanently true) while the agent's local rum_beacon_key stays
-- empty forever: no future operator save ever re-triggers a mint, the agent
-- injects no beacon, and the site collects zero RUM samples indefinitely with
-- no visible error anywhere. Recovery today requires a raw SQL fix.
--
-- This migration adds the two columns that let the CP tell the difference
-- between "never acked" and "acked, agent confirms it has the key" so a
-- config-ack reporting rum_beacon_present=false on an already-hash-set,
-- rum-enabled site can trigger an automatic re-mint (see the perf.Service
-- MarkConfigApplied / RotateBeaconKey / RumBeaconReconcileWorker Go-side
-- changes shipped in the same release). Mirrors m56's exact idempotent
-- add-column style (information_schema.columns existence check inside a
-- DO $$ block) so this migration is safe to re-run.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name   = 'site_perf_config'
          AND column_name  = 'beacon_key_acked_present'
    ) THEN
        ALTER TABLE "public"."site_perf_config"
            ADD COLUMN "beacon_key_acked_present" boolean NOT NULL DEFAULT false;
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name   = 'site_perf_config'
          AND column_name  = 'beacon_key_acked_at'
    ) THEN
        ALTER TABLE "public"."site_perf_config"
            ADD COLUMN "beacon_key_acked_at" timestamptz;
    END IF;
END;
$$;
