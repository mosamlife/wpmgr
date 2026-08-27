# `apps/api/db/analysis/`

Standalone, read-only operator SQL. **Nothing here is part of the application.**

## Why this directory exists and not `db/query/`

`apps/api/sqlc.yaml` sets `queries: "db/query"`, so sqlc parses *every* `.sql`
file in that directory. An analytics query dropped there either carries a
`-- name:` annotation — and then sqlc emits a Go method into
`internal/db/sqlc/**` that no caller ever invokes, in a tree CLAUDE.md forbids
hand-editing and that must stay a faithful projection of the application's real
query set — or it carries no annotation and sqlc's behaviour on it is not
something to rely on.

These files answer roadmap and capacity-planning questions for a human at a
`psql` prompt. They are not application queries, they have no Go call site, and
they must not grow one by accident. `db/analysis/` is a sibling of `db/query/`
and is outside sqlc's glob, so adding files here cannot change generated code.

Verify that claim rather than trusting it:

```sh
cd apps/api && sqlc compile && git status --porcelain internal/db/sqlc/
```

## Running these as the right role

`sites` is `ENABLE` **and** `FORCE ROW LEVEL SECURITY` with a RESTRICTIVE
`sites_site_scope` policy. `FORCE` means **the table owner does not bypass RLS
either** — only a role with `BYPASSRLS` or `SUPERUSER` does. So:

- **Per-organisation** (works as `wpmgr_app`, the role every install runs as):
  ```sh
  psql "$APP_DSN" -v tenant_id=<uuid> -f fleet_software_census.sql
  ```
  The script sets `app.tenant_id` itself so `sites_tenant_isolation` passes.

- **Fleet-wide across all tenants** requires a `BYPASSRLS` role — in practice
  the owner/migration DSN, not the application DSN:
  ```sh
  psql "$OWNER_DSN" -f fleet_software_census.sql
  ```
  Run fleet-wide as `wpmgr_app` and you get **zero rows, not an error**. The
  script detects that case and aborts rather than reporting an empty fleet as
  a finding.
