---
paths:
  - "apps/api/migrations/**"
  - "apps/api/db/**"
---

# Migrations and schema

This is `database-engineer`'s territory. If you are not that agent and you are
here, stop and route it.

## An applied migration is immutable

`internal/db/migrate.go` sorts the embedded versions lexically, tracks applied
versions in `schema_migrations`, and skips anything already present:

```go
sort.Strings(versions)
...
if applied[version] { continue }
```

Editing an applied migration is a silent no-op that looks like a fix. A
correction is a **new ordinal plus a converge path** for databases that ran the
earlier version. m114 and m115 exist only because of this.

A `PreToolUse` hook denies edits to migrations that already exist in `HEAD`.

## Ordinal is apply order, not commit order

m113's ordinal is `20260815000000` and it was committed after m114 and m115.
`migrate.go` does not verify `atlas.sum`, so a misordered file applies against
an unexpected base, inside `main()`, at boot. Read the ordinals of everything
unmerged before picking yours. Every statement idempotent.

## Site-keyed tables get the site-scope policy

The exemplar is `20260531050000_m19_orgs_sharing.sql`
(`grep -c 'AS RESTRICTIVE'` → 27). **Not m36** (→ 0). A new site-keyed table
needs, in the same migration: `tenant_id` plus its index, `ENABLE` **and**
`FORCE ROW LEVEL SECURITY`, the permissive `<table>_tenant_isolation` policy
with `WITH CHECK` mirroring `USING`, the `<table>_agent` policy, and the
**RESTRICTIVE `<table>_site_scope`** policy every sibling carries.

m112 exists because four tables shipped without it, so the database refused only
another tenant and not another site. Seven privilege-escalation doors were
closed in handlers before anyone asked why they kept appearing.

## `db/schema.sql` is not authoritative for RLS

Its first line calls itself the single source of truth. It is sqlc's input, and
it is well behind the migrations:

```
grep -rhoE 'CREATE POLICY "?[a-z_0-9]+_site_scope[a-z_0-9]*"?' apps/api/migrations/*.sql | sort -u | wc -l   # 46
grep -rhoE 'CREATE POLICY "?[a-z_0-9]+_site_scope[a-z_0-9]*"?' apps/api/db/schema.sql   | sort -u | wc -l   # 22
```

Grepping `schema.sql` to decide whether a table is site-scoped concludes it is
unprotected, which is the opposite of the truth. Grep both the quoted,
schema-qualified form the migrations use and the bare form `schema.sql` uses.

## Deletes take the lock; cascades destroy the record

The per-tenant key is `org_lifecycle` (`org.LifecycleLockKey`), taken with
`pg_advisory_xact_lock(hashtext($1), hashtext($2))` or the session-scoped
`pg_try_advisory_lock`/`pg_advisory_unlock`. A drain that skips it races a
restore that holds it and loses the restored chunks irreversibly.

`backup_chunks.tenant_id` is `ON DELETE CASCADE`, so deleting the tenant row
destroys the entire chunk inventory in the same statement and strands the
ciphertext. When you add a cascade, ask what reclaim or audit record dies with
it. That is m113 and m116.

## sqlc

`session-brief.sh` prints where `sqlc` is; do not assume the search path finds
it. The tree is stamped
`v1.31.1`; confirm with
`grep -h 'sqlc v' apps/api/internal/db/sqlc/*.go | sort -u`. Never hand-edit
`internal/db/sqlc/*.sql.go`; a hand-sync caused a production 500. Nothing in CI
or the Makefile runs or verifies sqlc.

## Prove the policy through the real path, as the real role

A test that opens its own connection never goes through the dispatch that sets
`app.site_scope`, so the policy is inert and the test is green. Go through the
repository layer, and connect as the `NOSUPERUSER NOBYPASSRLS` application role.

These proofs are in the integration package CI does not run
(`.claude/rules/ci-and-build-logic.md`). Run `make test-integration` locally,
and **commit before you start it**: it takes about nine minutes and an
interrupted run with uncommitted work loses everything.
