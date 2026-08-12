// reclaim_ops.go, GH #408 findings 2 and 3: the supported operator entry points
// into both reclaim engines.
//
// THE PROBLEM THESE SOLVE. site_object_reclaim is ENABLE + FORCE ROW LEVEL
// SECURITY. The remedy printed in the m113 header (and repeated in the
// CHANGELOG) and the UPDATE the site reclaim worker wrote into last_error were
// both authored against a superuser connection. Measured against real Postgres
// as wpmgr_app (NOSUPERUSER, NOBYPASSRLS) with no GUC set:
//
//	the documented INSERT is refused by the WITH CHECK, SQLSTATE 42501
//	the printed UPDATE is HIDDEN by the USING clause: rows=0, err=nil
//	SELECT count(*) returns 0, so the operator cannot even READ the table to
//	  discover the task id a correction needs
//
// The second is the worse of the three and is this project's signature defect:
// announcing success over having done nothing. The third is why documenting
// "SET app.tenant_id first" was not an adequate answer either.
//
// WHY THIS WORKS WITH NO SCHEMA CHANGE, AND WHY IT DOES NOT WIDEN ANY POLICY.
//
// site_object_reclaim_agent (m113) is already PERMISSIVE FOR ALL with BOTH
// USING and WITH CHECK, notably unlike backup_chunks_agent which is SELECT only,
// and db.InAgentTx sets app.agent='on' and NOTHING else. So every statement here
// runs on a connection the shipped policies already permit. No migration, no new
// database object, no policy edit.
//
// In particular this does NOT weaken m113's RESTRICTIVE site-scope policy, and
// that is measured rather than argued. InAgentTx leaves app.site_scope unset, so
// that policy's first branch is a tautology for this path, exactly as m113 says
// of its own two writers. With app.site_scope='on' and a non-matching
// app.allowed_site_ids the INSERT is still refused BY NAME
// (site_object_reclaim_site_scope), because RESTRICTIVE policies are
// AND-combined with permissive ones: the agent branch can only ever be
// intersected with site-scope, never unioned with it. A site-scoped collaborator
// is routed to InScopedTenantTx and has no shell in the API container either.
// TestGH408_SiteScopeStillRefusesTheAgentLane is the regression guard.
//
// A SECURITY DEFINER function was the obvious alternative and is rejected
// because its security property DIFFERS BY DEPLOYMENT: owned by a non-superuser
// it is refused under FORCE RLS unless it sets the GUC in-body, but owned by a
// SUPERUSER, which is the default self-host and dev deployment (the compose file
// runs migrations as the bootstrap superuser and config.MigrateDSN falls back to
// the app DSN), it bypasses ALL RLS INCLUDING the restrictive site-scope policy.
// A security property that differs by deployment is not a security property.
//
// EVERY FUNCTION HERE REPORTS ROWS AFFECTED, and every caller treats zero as a
// failure. That property, not the GUC, is what makes this a recovery path rather
// than another thing that reports success having done nothing.
package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// operatorListLimit bounds what `reclaim list` reads in one go. Large enough to
// show a real backlog, small enough that a runaway table cannot fill a terminal.
const operatorListLimit = 500

// ErrTenantStillExists refuses a hand-typed tenant backfill for a tenant that is
// still live.
//
// The worker checks this too (GUARD 2), and this is not redundant with it: this
// is the ONE command that grants chunk-delete authority from an argument typed
// by a human, so the guard belongs in the operator's hands at the moment of
// typing as well as in the drain.
var ErrTenantStillExists = errors.New("a tenants row with that id still exists")

// ObjectLister is the read-only half of object storage. `reclaim discover` needs
// nothing else, and deliberately cannot delete.
type ObjectLister interface {
	List(ctx context.Context, prefix string) ([]string, error)
}

// OpenReclaims is what `reclaim list` shows.
type OpenReclaims struct {
	Sites   []ReclaimTask
	Tenants []TenantReclaimTask
}

// ListOpenReclaims reads every open task in both engines.
//
// This is the answer to the chicken-and-egg that disqualified the "document the
// GUC" option: with no GUC the tables read as empty, so an operator could not
// discover the id that instruction would have required them to supply.
func ListOpenReclaims(ctx context.Context, pool *db.Pool) (OpenReclaims, error) {
	var out OpenReclaims
	err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		siteRows, err := q.ListOpenSiteObjectReclaims(ctx, operatorListLimit)
		if err != nil {
			return fmt.Errorf("list open site tasks: %w", err)
		}
		out.Sites = reclaimTasksFromRows(siteRows)
		tenantRows, terr := q.ListOpenTenantObjectReclaims(ctx, operatorListLimit)
		if terr != nil {
			return fmt.Errorf("list open tenant tasks: %w", terr)
		}
		out.Tenants = tenantReclaimTasksFromRows(tenantRows)
		return nil
	})
	return out, err
}

// EnqueueSiteReclaim hands a known-deleted site to the m113 engine (GH #408
// finding 2). It is the statement the m113 header documented, run on the
// connection a self-hoster actually has.
//
// Backfilling a site whose row still EXISTS is allowed here on purpose and is
// not a hole: the worker's GUARD 3 re-reads the sites table and CANCELS such a
// task without touching storage, which is the one outcome that proves there is
// nothing to reclaim. The tenant command is the one that refuses up front,
// because that one grants chunk-delete authority.
func EnqueueSiteReclaim(ctx context.Context, pool *db.Pool, tenantID, siteID uuid.UUID) (int64, error) {
	if tenantID == uuid.Nil || siteID == uuid.Nil {
		return 0, errors.New("reclaim site: a nil tenant or site id would name the wrong storage prefix")
	}
	var rows int64
	err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).EnqueueSiteObjectReclaim(ctx, sqlc.EnqueueSiteObjectReclaimParams{
			TenantID: tenantID,
			SiteID:   siteID,
			Kind:     ReclaimKindBackupManifest,
			// Diagnostic only, and unknowable for a site whose row is long gone.
			DestinationKind: nil,
		})
		rows = n
		return err
	})
	return rows, err
}

// EnqueueTenantReclaim hands an already hard-deleted tenant to the m116 drain
// (GH #408 finding 1, for tenants deleted before m116 existed).
//
// REFUSES if a tenants row with that id still exists, before enqueueing
// anything. See ErrTenantStillExists.
func EnqueueTenantReclaim(ctx context.Context, pool *db.Pool, tenantID uuid.UUID) (int64, error) {
	if tenantID == uuid.Nil {
		return 0, errors.New("reclaim tenant: a nil tenant id would name the wrong storage prefix")
	}
	var rows int64
	err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		exists, eerr := q.TenantExistsForReclaim(ctx, tenantID)
		if eerr != nil {
			return fmt.Errorf("check the tenant is really gone: %w", eerr)
		}
		if exists {
			return fmt.Errorf("%w (%s): its storage belongs to a live organisation", ErrTenantStillExists, tenantID)
		}
		n, err := q.EnqueueTenantObjectReclaim(ctx, sqlc.EnqueueTenantObjectReclaimParams{
			TenantID: tenantID,
			Kind:     TenantReclaimKindStorage,
		})
		rows = n
		return err
	})
	return rows, err
}

// reclaimRetryCommand is the ONE construction of the command an operator pastes,
// for either engine.
//
// Every place that advice is produced goes through this function: the site
// worker's guard-1 failure reason and its every-tick stuck report, and the tenant
// worker's guard-1 failure reason, stuck report and per-attempt failure log.
// Before this there were three independent constructions of the same string, one
// of them an inline literal in the site worker's stuck report, under a comment
// claiming they could not drift apart. This string is what an operator copies,
// and a stale one sends them to a command that does not exist.
//
// kind names the engine the task lives in, so each table's advice corrects its
// own row rather than relying on a default that is only right for one of them.
// Both kinds are accepted by RetryReclaimTask (see classifyRetryKind), which
// they were not: the tenant hint omitted --kind, so the site-only validation
// there was never met by the printed command and the defect stayed latent.
func reclaimRetryCommand(taskID uuid.UUID, kind string) string {
	return "wpmgr-cli reclaim retry --task " + taskID.String() + " --kind " + kind
}

// reclaimRetryAdvice is reclaimRetryCommand wrapped in the sentence a FAILED task
// carries in last_error, and is deliberately NOT a SQL statement (GH #408
// finding 3).
//
// The wording holds for both engines: neither cancels a task it merely could not
// do, because in both tables the row is the last record naming objects that are
// still in storage.
func reclaimRetryAdvice(taskID uuid.UUID, kind string) string {
	return " (the task is kept and retried, never cancelled: its objects are still in storage and " +
		"this row is the only record of them. Correct it with: " + reclaimRetryCommand(taskID, kind) +
		" which runs as the ordinary application role and exits non-zero if it changes nothing)"
}

// retryEngine is which reclaim table a --kind names.
type retryEngine struct {
	// siteKind is the kind a matching SITE task is corrected to. Empty when the
	// operator named the tenant engine, and the site table is then not touched
	// at all.
	siteKind string
	// tenantOnly is set when the kind belongs to the tenant engine alone.
	tenantOnly bool
}

// classifyRetryKind resolves the --kind an operator supplied against BOTH closed
// kind sets.
//
// It used to check the site set alone, so `reclaim retry --kind tenant_storage`
// was refused before the tenant table was ever consulted: the one command family
// that exists to dig an operator out of a hole rejected its own engine's kind.
// The hints omitted --kind, so the default path worked and nothing noticed. Both
// hints now name their own kind, and TestGH408_PrintedHintsAreAcceptedByRetry
// fails if either prints a kind this refuses.
//
// An unknown kind is refused naming both valid sets, because the realistic
// caller is an operator typing under pressure at the one moment they need a
// working command.
func classifyRetryKind(kind string) (retryEngine, error) {
	switch {
	case kind == "":
		// The default is the site engine's only kind. A tenant task still
		// reopens under it, because the tenant table is consulted first and its
		// reopen does not touch kind at all.
		return retryEngine{siteKind: ReclaimKindBackupManifest}, nil
	case KnownReclaimKind(kind):
		return retryEngine{siteKind: kind}, nil
	case KnownTenantReclaimKind(kind):
		return retryEngine{tenantOnly: true}, nil
	default:
		return retryEngine{}, fmt.Errorf(
			"reclaim retry: unknown kind %q. Valid kinds are %v for a site task and %v for an "+
				"organisation task", kind, ReclaimKinds, TenantReclaimKinds)
	}
}

// RetryReclaimTask reopens a stuck task in either engine (GH #408 finding 3).
//
// It replaces the bare UPDATE the site reclaim worker used to print. taskID is
// looked up in the tenant table first and then the site table; kind is applied
// only to a site task, where a mistyped kind is the realistic reason a task is
// stuck in the first place. An empty kind means the default, and a tenant kind
// confines the lookup to the tenant table. Returns the number of rows actually
// changed, which the caller turns into an exit code.
func RetryReclaimTask(ctx context.Context, pool *db.Pool, taskID uuid.UUID, kind string) (int64, error) {
	if taskID == uuid.Nil {
		return 0, errors.New("reclaim retry: a nil task id matches nothing")
	}
	engine, kerr := classifyRetryKind(kind)
	if kerr != nil {
		return 0, kerr
	}
	var rows int64
	err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		n, terr := q.ReopenTenantObjectReclaim(ctx, taskID)
		if terr != nil {
			return fmt.Errorf("reopen tenant task: %w", terr)
		}
		if n > 0 {
			rows = n
			return nil
		}
		if engine.tenantOnly {
			// The operator named the tenant engine explicitly and no open
			// tenant task has that id. Falling through would aim a site UPDATE
			// at it carrying a kind the site table cannot hold, and the answer
			// an operator needs here is which table they are actually in.
			return fmt.Errorf("reclaim retry: --kind %s names organisation storage, but no OPEN "+
				"organisation task has id %s. `wpmgr-cli reclaim list` shows every open task in "+
				"both engines", kind, taskID)
		}
		n, serr := q.ReopenSiteObjectReclaim(ctx, sqlc.ReopenSiteObjectReclaimParams{ID: taskID, Kind: engine.siteKind})
		if serr != nil {
			return fmt.Errorf("reopen site task: %w", serr)
		}
		rows = n
		return nil
	})
	return rows, err
}

// BackfillHardDeletedTenants enqueues a drain for every tenant Lane A hard
// deleted BEFORE m116 existed, recovered from the database rather than from a
// bucket scan.
//
// DELETE /orgs/{orgId} writes org.deleted to system_audit_log with the lane in
// its metadata, and m93 gave that table a tenant_id column with NO foreign key
// to tenants precisely so the record survives the delete it describes. So the
// whole DELETE /orgs Lane A population is recoverable from the database, is
// evidence-based rather than guessed, and every candidate still passes all four
// of the drain's guards.
//
// It does NOT cover the superadmin orphan cleanup, which writes no system audit
// event at all: those tenant ids exist nowhere in the database. Discover is the
// report-only route for those, and it keeps a human in the loop on purpose.
func BackfillHardDeletedTenants(ctx context.Context, pool *db.Pool) ([]uuid.UUID, error) {
	var enqueued []uuid.UUID
	err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		ids, lerr := q.ListHardDeletedTenantsFromSystemAudit(ctx)
		if lerr != nil {
			return fmt.Errorf("read the hard-delete audit trail: %w", lerr)
		}
		for _, id := range ids {
			if id == uuid.Nil {
				continue
			}
			n, eerr := q.EnqueueTenantObjectReclaim(ctx, sqlc.EnqueueTenantObjectReclaimParams{
				TenantID: id,
				Kind:     TenantReclaimKindStorage,
			})
			if eerr != nil {
				return fmt.Errorf("enqueue %s: %w", id, eerr)
			}
			if n > 0 {
				enqueued = append(enqueued, id)
			}
		}
		return nil
	})
	return enqueued, err
}

// DiscoverCandidate is one bucket prefix whose tenant no longer exists.
type DiscoverCandidate struct {
	TenantID uuid.UUID
	Roots    []string
	Keys     int
}

// DiscoverOrphanTenantPrefixes LISTS the two tenant-namespaced roots that carry
// the bulk of a tenant's bytes and reports the ids with no tenants row.
//
// REPORT ONLY. It never deletes and never enqueues, and that is a deliberate
// design choice rather than caution: enqueueing from a bucket listing would
// invert this design's direction of trust and make a full-root LIST the input to
// an irreversible delete. The safe direction is the one m116 has, where a
// database OLDER than the bucket holds no record and does nothing, and only a
// database that genuinely performed the delete holds an instruction. So a human
// reads this output and feeds an id to `reclaim tenant` themselves, where all
// four drain guards still apply to it.
//
// It exists for the ONE population the database cannot name: tenants removed by
// the superadmin orphan cleanup, which writes no system audit event.
func DiscoverOrphanTenantPrefixes(ctx context.Context, pool *db.Pool, store ObjectLister) ([]DiscoverCandidate, error) {
	if store == nil {
		return nil, errors.New("reclaim discover: no object storage is configured")
	}
	// The two roots that name a tenant unambiguously and hold the bytes worth
	// finding. Note the SINGULAR "tenant/": the plural "tenants/" root holds
	// white-label client report PDFs with client PII, and the two differ by one
	// character.
	roots := []string{"chunks/", "tenant/"}
	seen := map[uuid.UUID]*DiscoverCandidate{}
	for _, root := range roots {
		keys, err := store.List(ctx, root)
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", root, err)
		}
		for _, k := range keys {
			id, ok := tenantIDFromKey(root, k)
			if !ok {
				continue
			}
			c, present := seen[id]
			if !present {
				c = &DiscoverCandidate{TenantID: id}
				seen[id] = c
			}
			c.Keys++
			if !containsString(c.Roots, root+id.String()+"/") {
				c.Roots = append(c.Roots, root+id.String()+"/")
			}
		}
	}
	var out []DiscoverCandidate
	err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		for id, c := range seen {
			exists, eerr := q.TenantExistsForReclaim(ctx, id)
			if eerr != nil {
				return fmt.Errorf("check tenant %s: %w", id, eerr)
			}
			if exists {
				continue // a live organisation's storage, correctly in the bucket
			}
			out = append(out, *c)
		}
		return nil
	})
	return out, err
}

// tenantIDFromKey parses the uuid segment immediately after root, STRICTLY. A
// key that does not parse is skipped rather than guessed at: the output of this
// function is a candidate an operator may hand to a delete.
func tenantIDFromKey(root, key string) (uuid.UUID, bool) {
	if !strings.HasPrefix(key, root) {
		return uuid.Nil, false
	}
	rest := key[len(root):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return uuid.Nil, false
	}
	seg := rest[:slash]
	if len(seg) != 36 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(seg)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// FormatTaskAge renders how long a task has been waiting. `reclaim list` prints
// it for every open task in both engines (cmd/wpmgr-cli/reclaim.go).
//
// It is there because attempts alone does not tell an operator whether a task is
// stuck: attempts=8 an hour after a storage outage is a task working through its
// backoff, and attempts=8 three months old is a prefix nothing will ever reclaim
// and a bill that has been running since. Both give up at the same cap and read
// identically without this.
func FormatTaskAge(since time.Time) string {
	d := time.Since(since).Round(time.Minute)
	if d < time.Minute {
		return "just now"
	}
	return d.String() + " ago"
}
