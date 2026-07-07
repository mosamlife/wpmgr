-- M95 — managed-backup-storage grandfather (M16 Phase B).
--
-- Ships DARK behind WPMGR_HOSTED (default false): self-host and current prod
-- see ZERO behavior change until the flag is turned on AND
-- internal/backup.Service.SetBillingGate's caller actually resolves a
-- managed destination for a run. This migration only ever writes a
-- plan_overrides delta key; it never itself gates anything.
--
-- internal/billing.Entitlements.ManagedBackupStorage is false on the free
-- tier (a free-plan tenant must configure a local-folder or S3-compatible
-- backup destination instead of the CP-managed bucket, per
-- internal/backup.Service.CheckManagedBackupStorage). Applied with no
-- grandfather, that rule would silently cut off every EXISTING free-plan
-- tenant's managed backups the instant WPMGR_HOSTED is turned on for M16
-- Phase B — including tenants who have relied on managed storage for months
-- with no BYO destination configured. Grandfathering (mirrors m91's max_sites
-- backfill exactly): every tenant that exists AT MIGRATION TIME gets an
-- explicit plan_overrides.managed_backup_storage = true, so nobody loses
-- capability they already had; the gate only ever applies to NEW growth — a
-- tenant created AFTER this migration runs with no override, so a free-plan
-- newcomer is correctly gated the moment Phase B activates.
--
-- Non-destructive / idempotent: jsonb_set with the "NOT (plan_overrides ?
-- 'managed_backup_storage')" guard merges the new key into whatever
-- plan_overrides already holds (e.g. a m91 max_sites override survives
-- untouched) and makes a second run of this migration (or a fresh apply
-- against a re-migrated dev DB) a no-op once the key is set.
--
-- Vendor-neutral: no payment-provider or competitor naming, matching every
-- other billing migration in this tree.

UPDATE "public"."tenants"
SET "plan_overrides" = jsonb_set(plan_overrides, '{managed_backup_storage}', 'true'::jsonb, true)
WHERE NOT (plan_overrides ? 'managed_backup_storage');
