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

Verify that claim rather than trusting it — with a check that can actually fail:

```sh
cd apps/api
sqlc generate
git diff --quiet -- internal/db/sqlc/ || { echo "generated tree moved"; exit 1; }
```

The obvious version of this check is worthless, and it shipped here first:

```sh
cd apps/api && sqlc compile && git status --porcelain internal/db/sqlc/   # proves nothing
```

`sqlc compile` only type-checks; it writes no output, so nothing under
`internal/db/sqlc/` *can* change during it and the following check has nothing
to observe. And `git status --porcelain` exits 0 whether or not it prints a
path, so even real drift would leave the pipeline green. `sqlc generate` makes a
change possible and `git diff --quiet` exits 1 exactly when one happened — the
same gate `.claude/rules/db-migrations.md` requires.

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

## Schema requirement

`fleet_software_census.sql` reads `sites.components_updated_at`, added by
**m121** (`20260823000000_m121_site_components_updated_at`). Against an older
database both the census and its proof harness fail on the unknown column,
loudly, which is the correct outcome: the census dates every freshness number
from that column and cannot be run — or proven — against a schema that lacks it.

m121 added the column with **no backfill**, so every row that predates it reads
`NULL` until that site's next metadata push. `NULL` means "we have never
recorded when this inventory was collected", it is reported as its own bucket,
and on a run soon after m121 it is expected to be the largest bucket — how large
depends on how many sites have pushed since, so read the number the run prints
rather than assuming one. Read section 0 of the output, which prints that
denominator before any adoption number, before quoting anything further down.

### What the timestamp actually records

`components_updated_at` is `now()` evaluated **on the control plane at the moment
it persisted the inventory document**. It is *not* the instant the agent walked
the plugin and theme list on the WordPress host.

The agent's metadata payload carries no collection timestamp, so the collection
instant is not knowable here at all — which is why m121 named the column
`components_updated_at` and deliberately not `components_collected_at`
(m121 DECISION 2). The two are separated by one agent push: normally a single
HTTP round trip, and not small when it matters, since a queued or retried push,
a clock-skewed host or any future store-and-forward path widens it without
warning.

So every freshness figure the census prints means "how long since **we recorded**
this", which is a lower bound on "how long since the agent **looked**". Like
every other error in this script it runs one way: the inventory can be older
than reported, never newer.
