---
paths:
  - "apps/api/**/*.go"
---

# Control plane

Traps this package has actually sprung, not general Go advice.
Schema, migrations and `db/query/**` are covered separately by
`.claude/rules/db-migrations.md` and belong to `database-engineer`.

## RLS lives in the tx helpers, never in a handler

`internal/db/db.go` owns them: `InTenantTx`, `InTenantTxAsUser`, `InUserTx`,
`InScopedTenantTx` (the per-site-sharing path that sets `app.site_scope` and
`app.allowed_site_ids`), and the narrow lookup scopes `InEnrollTx`,
`InAgentTx`, `InAPIKeyLookupTx`, `InInviteLookupTx`. A tenant query outside one
of these has no RLS context.

**A test that opens its own connection goes around all of it.** m112's proofs
did exactly that, so every policy was inert and every test was green. Reach the
database through the same helper the request uses, and connect as the
non-superuser, non-`BYPASSRLS` application role.

Queries still carry an explicit `tenant_id` in `WHERE`/`VALUES` even under RLS:
defence in depth, and it keeps the index in play.

## Never hand-edit generated trees

`internal/db/sqlc/**` and `internal/api/gen/**` are machine output. A hand-sync
of the sqlc tree caused a production 500. Regenerate.

`make gen` and `scripts/gen-openapi.sh` are **stubs**, the script prints one
line and changes nothing, so a `git status` assertion after running it can never
fail. The real regeneration is `go generate ./internal/api/gen/...` plus
`pnpm -C packages/openapi-client generate`, committed together with the
`openapi.yaml` change.

## Cursor pagination

Lists ordered by `created_at` must use the composite `(created_at, id) <`
predicate. Batch inserts share a `created_at` and a bare compare silently skips
co-timestamped rows. The `, id` tiebreaker in `ORDER BY` is the convention
everywhere.

## Guard with `CASE`, not a regex before a cast

Never guard a value by regex before casting it in a `LEFT JOIN`, use a `CASE`
guard. The regex-before-`::uuid` pattern 500'd the audit log.

## Two writers on one row

Where the control plane and the agent both write the same row, an unguarded
upsert lets a stale agent push regress a fresh control-plane stamp. Use
`GREATEST(EXCLUDED.x, table.x)` with a `CASE` for the companion column, and keep
operator-config writes and agent-reported writes in separate queries.

## Deletes and reclamation

Every delete of scratch or object storage is gated on the live run-lock. A
missing DB row is not proof the run is dead, and mtime lies. The per-tenant key
is `org_lifecycle` (`org.LifecycleLockKey`); a drain that skips it races a
restore that holds it and the restored chunks are gone. Route this work to
`security-reviewer`.

## Integration tests do not run in CI

The integration package is excluded from CI by name and its workflow is
manual-dispatch; `.claude/rules/ci-and-build-logic.md` names it and says why.
That is where the tenancy and RLS proofs live, so run `make test-integration`
locally before merging anything touching RLS, the email domain, tenant scoping,
or object-storage reclamation. A regression there merges green.

It takes about nine minutes. **Commit before you start it.**
