// reclaim.go, GH #408 findings 2 and 3: the supported operator entry point into
// both object-storage reclaim engines.
//
// WHY THIS EXISTS AT ALL. The remedies shipped with GH #402 were authored
// against a superuser connection and do not work as wpmgr_app, the NOSUPERUSER
// NOBYPASSRLS role this repository provisions for the application and the role a
// self-hoster actually has. Measured against real Postgres with no GUC set: the
// documented backfill INSERT is refused (SQLSTATE 42501), the UPDATE the reclaim
// worker printed into last_error is HIDDEN by the RLS USING clause and reports
// success having changed nothing (rows=0, err=nil), and SELECT count(*) returns
// 0, so the operator cannot even read the table to find the id a hand-written
// correction would need.
//
// Every subcommand here runs its statements through db.Pool.InAgentTx against
// the APP DSN, which the shipped m113/m116 _agent policies already permit, and
// exits NON-ZERO when it affected zero rows. That exit code, not the GUC, is
// what makes this a recovery path rather than another thing that reports success
// having done nothing. See internal/backup/reclaim_ops.go for why this does not
// widen m113's restrictive site-scope policy, and why a SECURITY DEFINER
// function was rejected.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// errNothingChanged is what every mutating subcommand returns when it affected
// zero rows. main turns it into a non-zero exit.
//
// It is a distinct error rather than a printed warning because the whole defect
// being fixed is a remedy that returns success having done nothing. A recovery
// tool that can do that is not a recovery tool.
var errNothingChanged = errors.New("nothing changed: no row matched, so nothing was recorded and no storage will be reclaimed")

func reclaimUsage(w io.Writer) {
	fmt.Fprintln(w, "wpmgr-cli reclaim: object-storage reclamation (GH #402, GH #408)")
	fmt.Fprintln(w, "  list                                  show every open reclamation task")
	fmt.Fprintln(w, "  site   --tenant <uuid> --site <uuid>  reclaim a deleted site's backup manifests")
	fmt.Fprintln(w, "  tenant --tenant <uuid>                reclaim an already-deleted organisation's storage")
	fmt.Fprintln(w, "  retry  --task <uuid> [--kind <kind>]  reopen a stuck task")
	fmt.Fprintln(w, "  backfill-tenants                      enqueue every organisation the audit trail shows was hard deleted")
	fmt.Fprintln(w, "  discover --report-only                list bucket prefixes whose organisation no longer exists (never deletes)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Runs as the ordinary application role. Exits non-zero if it changes nothing.")
}

// reclaimCmd dispatches the reclaim subcommand family. args is everything after
// "reclaim".
func reclaimCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		reclaimUsage(os.Stderr)
		return errors.New("reclaim: a subcommand is required")
	}
	ctx := context.Background()
	sub, rest := args[0], args[1:]

	switch sub {
	case "list":
		pool, err := openAppPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		return reclaimList(ctx, pool, out)

	case "site":
		fs := flag.NewFlagSet("reclaim site", flag.ContinueOnError)
		tenant := fs.String("tenant", "", "the organisation's uuid")
		siteID := fs.String("site", "", "the deleted site's uuid")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		tid, terr := uuid.Parse(*tenant)
		if terr != nil {
			return fmt.Errorf("reclaim site: --tenant is not a valid uuid: %w", terr)
		}
		sid, serr := uuid.Parse(*siteID)
		if serr != nil {
			return fmt.Errorf("reclaim site: --site is not a valid uuid: %w", serr)
		}
		pool, err := openAppPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		rows, err := backup.EnqueueSiteReclaim(ctx, pool, tid, sid)
		if err != nil {
			return err
		}
		if rows == 0 {
			return errNothingChanged
		}
		fmt.Fprintf(out, "recorded: the sweep will reclaim tenant/%s/site/%s/\n", tid, sid)
		return nil

	case "tenant":
		fs := flag.NewFlagSet("reclaim tenant", flag.ContinueOnError)
		tenant := fs.String("tenant", "", "the deleted organisation's uuid")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		tid, terr := uuid.Parse(*tenant)
		if terr != nil {
			return fmt.Errorf("reclaim tenant: --tenant is not a valid uuid: %w", terr)
		}
		pool, err := openAppPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		rows, err := backup.EnqueueTenantReclaim(ctx, pool, tid)
		if err != nil {
			return err
		}
		if rows == 0 {
			return errNothingChanged
		}
		fmt.Fprintf(out, "recorded: the drain will reclaim every storage root of organisation %s,\n", tid)
		fmt.Fprintln(out, "including its deduplicated backup chunks. It re-checks that the organisation,")
		fmt.Fprintln(out, "its sites and its chunk inventory are all really gone before deleting anything.")
		return nil

	case "retry":
		fs := flag.NewFlagSet("reclaim retry", flag.ContinueOnError)
		task := fs.String("task", "", "the task uuid, as shown by `reclaim list`")
		// Both engines' kinds are accepted here. The tenant one used to be
		// refused before the tenant table was consulted at all, which made the
		// one command family that exists to dig an operator out of a hole reject
		// its own kind (GH #408 review).
		kind := fs.String("kind", "",
			"the task's kind: "+backup.ReclaimKindBackupManifest+" for a site task, which a site "+
				"task is also corrected to, or "+backup.TenantReclaimKindStorage+" for an "+
				"organisation task (default "+backup.ReclaimKindBackupManifest+")")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		tid, terr := uuid.Parse(*task)
		if terr != nil {
			return fmt.Errorf("reclaim retry: --task is not a valid uuid: %w", terr)
		}
		pool, err := openAppPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		rows, err := backup.RetryReclaimTask(ctx, pool, tid, *kind)
		if err != nil {
			return err
		}
		if rows == 0 {
			return errNothingChanged
		}
		fmt.Fprintf(out, "reopened task %s; the next sweep will retry it\n", tid)
		return nil

	case "backfill-tenants":
		pool, err := openAppPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		ids, berr := backup.BackfillHardDeletedTenants(ctx, pool)
		if berr != nil {
			return berr
		}
		if len(ids) == 0 {
			return errNothingChanged
		}
		fmt.Fprintf(out, "recorded %d organisation(s) the audit trail shows were hard deleted:\n", len(ids))
		for _, id := range ids {
			fmt.Fprintf(out, "  %s\n", id)
		}
		return nil

	case "discover":
		fs := flag.NewFlagSet("reclaim discover", flag.ContinueOnError)
		reportOnly := fs.Bool("report-only", false, "required: this command never deletes and never enqueues")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if !*reportOnly {
			return errors.New("reclaim discover: pass --report-only. This command reads the bucket and " +
				"prints candidates; it never deletes and never enqueues, and the flag is required so " +
				"that is unambiguous at the moment you run it")
		}
		pool, err := openAppPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		store, serr := openObjectStore()
		if serr != nil {
			return serr
		}
		found, derr := backup.DiscoverOrphanTenantPrefixes(ctx, pool, store)
		if derr != nil {
			return derr
		}
		if len(found) == 0 {
			fmt.Fprintln(out, "no bucket prefixes found whose organisation is missing")
			return nil
		}
		fmt.Fprintf(out, "%d organisation(s) have storage in the bucket but no row in the database.\n", len(found))
		fmt.Fprintln(out, "Nothing has been deleted or recorded. Check each one against your own records,")
		fmt.Fprintln(out, "then hand the ones you are sure about to: wpmgr-cli reclaim tenant --tenant <uuid>")
		for _, c := range found {
			fmt.Fprintf(out, "  %s  %d object(s) under %v\n", c.TenantID, c.Keys, c.Roots)
		}
		return nil

	default:
		reclaimUsage(os.Stderr)
		return fmt.Errorf("reclaim: unknown subcommand %q", sub)
	}
}

func reclaimList(ctx context.Context, pool *db.Pool, out io.Writer) error {
	open, err := backup.ListOpenReclaims(ctx, pool)
	if err != nil {
		return err
	}
	if len(open.Sites) == 0 && len(open.Tenants) == 0 {
		fmt.Fprintln(out, "no open reclamation tasks")
		return nil
	}
	// age is printed next to attempts on purpose. The two answer different
	// questions and only together say whether a task is stuck: attempts=8 an
	// hour old is a task working through its backoff after a storage outage,
	// attempts=8 three months old is a prefix nothing will ever reclaim and a
	// bill that has been running since.
	if len(open.Tenants) > 0 {
		fmt.Fprintf(out, "organisation storage (%d):\n", len(open.Tenants))
		for _, t := range open.Tenants {
			fmt.Fprintf(out, "  task=%s organisation=%s attempts=%d age=%s\n",
				t.ID, t.TenantID, t.Attempts, backup.FormatTaskAge(t.CreatedAt))
			if t.LastError != "" {
				fmt.Fprintf(out, "    last error: %s\n", t.LastError)
			}
		}
	}
	if len(open.Sites) > 0 {
		fmt.Fprintf(out, "site manifests (%d):\n", len(open.Sites))
		for _, s := range open.Sites {
			fmt.Fprintf(out, "  task=%s organisation=%s site=%s kind=%s attempts=%d age=%s\n",
				s.ID, s.TenantID, s.SiteID, s.Kind, s.Attempts, backup.FormatTaskAge(s.CreatedAt))
			if s.LastError != "" {
				fmt.Fprintf(out, "    last error: %s\n", s.LastError)
			}
		}
	}
	return nil
}

// openAppPool connects with the APPLICATION DSN, deliberately NOT MigrateDSN.
//
// That is the point of this whole command family: it must work for the role a
// self-hoster has, so it must not quietly reach for an owner connection when one
// happens to be configured. A statement that only works on a privileged
// connection is the defect being fixed.
func openAppPool(ctx context.Context) (*db.Pool, error) {
	cfg, err := config.Load(os.Getenv("WPMGR_CONFIG_FILE"))
	if err != nil {
		return nil, err
	}
	return db.Connect(ctx, cfg.DB.DSN())
}

func openObjectStore() (*blobstore.Store, error) {
	cfg, err := config.Load(os.Getenv("WPMGR_CONFIG_FILE"))
	if err != nil {
		return nil, err
	}
	if !cfg.S3.Enabled() {
		return nil, errors.New("reclaim discover: no object storage is configured for this control plane")
	}
	return blobstore.New(blobstore.Config{
		Endpoint:       cfg.S3.Endpoint,
		Region:         cfg.S3.Region,
		Bucket:         cfg.S3.Bucket,
		AccessKey:      cfg.S3.AccessKey,
		SecretKey:      cfg.S3.SecretKey,
		ForcePathStyle: cfg.S3.ForcePathStyle,
	})
}
