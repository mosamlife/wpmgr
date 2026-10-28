package email

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// SiteEmailLockKey namespaces the per-site lock that excludes, from each
// other, every operation that sends a site's email config to its agent or
// removes it (GH #968): the removal, a save's push, the send-test pre-sync,
// Sync to site, and a connection save. PropagateOrgConfig does not take it:
// it sends only the organisation default, and only to sites with no row of
// their own.
// It is taken as pg_try_advisory_lock(hashtext(key), hashtext(site id)) on one
// pinned connection, so it excludes across every API instance.
//
// Without it a push of the row as it already stood, in flight when a removal
// runs, lands on the site after the removal cleared it: the dashboard says the
// provider is gone while the site still holds it and its credential.
const SiteEmailLockKey = "email_site_config"

// siteLockWait bounds how long an operation waits for another email change on
// the same site to finish. A removal holds the lock across two agent calls.
const siteLockWait = 10 * time.Second

// siteLockPoll is how often a waiting operation asks for the lock again. The
// pooled connection goes back to the pool between asks, so a waiter holds no
// connection while it waits.
const siteLockPoll = 100 * time.Millisecond

// errSiteEmailBusy is returned by a siteLocker when the wait ran out.
var errSiteEmailBusy = errors.New("email: another email change for this site is in progress")

// siteBusyMessage is the operator's answer when the wait ran out. Nothing was
// changed.
const siteBusyMessage = "Another email change for this site is in progress. Try again in a moment."

// siteBusyError is the 409 for a wait that ran out.
func siteBusyError() *domain.Error {
	return domain.Conflict("email_change_in_progress", siteBusyMessage)
}

// siteLocker takes the per-site email lock.
type siteLocker interface {
	// lockSite returns once the site's lock is held, errSiteEmailBusy when the
	// bounded wait ran out, or the error that stopped it. release must be
	// called exactly once, on every exit; extra calls are no-ops.
	lockSite(ctx context.Context, siteID uuid.UUID) (release func(), err error)
}

// lockSiteForChange takes the site's lock and turns a failure into the domain
// error a caller returns as is: 409 when the wait ran out, 503 otherwise. In
// both cases nothing has been changed. The error is returned as the concrete
// type so a caller can read its Message; callers return it only when non-nil.
func (s *Service) lockSiteForChange(ctx context.Context, siteID uuid.UUID) (func(), *domain.Error) {
	release, err := s.locks.lockSite(ctx, siteID)
	if err == nil {
		return release, nil
	}
	if errors.Is(err, errSiteEmailBusy) {
		return nil, siteBusyError()
	}
	return nil, domain.ServiceUnavailable("email_site_lock_failed",
		"Could not start the email change for this site; nothing was changed. Try again.").WithCause(err)
}

// pgSiteLocker is the production siteLocker: a session-level Postgres advisory
// lock held on one pinned pooled connection for the whole operation. A
// session lock, not a transaction lock, because the operation spans agent
// calls, and a transaction left open across them is cut by the server's
// idle-in-transaction timeout.
type pgSiteLocker struct {
	pool *db.Pool
	wait time.Duration
	poll time.Duration
}

func (l *pgSiteLocker) lockSite(ctx context.Context, siteID uuid.UUID) (func(), error) {
	deadline := time.Now().Add(l.wait)
	for {
		conn, err := l.pool.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		var got bool
		if err := conn.QueryRow(ctx,
			`SELECT pg_try_advisory_lock(hashtext($1), hashtext($2))`,
			SiteEmailLockKey, siteID.String(),
		).Scan(&got); err != nil {
			// The server may have taken the lock before the reply was lost, so
			// the session is closed rather than pooled: ending it drops every
			// lock it holds.
			discardLockConn(ctx, conn)
			return nil, err
		}
		if got {
			var once sync.Once
			return func() { once.Do(func() { unlockSiteConn(ctx, conn, siteID) }) }, nil
		}
		conn.Release()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, errSiteEmailBusy
		}
		timer := time.NewTimer(min(l.poll, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// unlockSiteConn releases the site's lock and returns the connection to the
// pool. It runs on db.CleanupContext, so a cancelled request still puts the
// unlock on the wire. When the unlock fails, or reports that this session did
// not hold the lock, the connection is closed instead of pooled.
func unlockSiteConn(ctx context.Context, conn *pgxpool.Conn, siteID uuid.UUID) {
	cctx, cancel := db.CleanupContext(ctx)
	defer cancel()
	var released bool
	err := conn.QueryRow(cctx,
		`SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`,
		SiteEmailLockKey, siteID.String(),
	).Scan(&released)
	if err != nil || !released {
		_ = conn.Conn().Close(cctx)
	}
	conn.Release()
}

// discardLockConn closes a pinned connection whose lock state is unknown and
// hands it back, which destroys it.
func discardLockConn(ctx context.Context, conn *pgxpool.Conn) {
	cctx, cancel := db.CleanupContext(ctx)
	defer cancel()
	_ = conn.Conn().Close(cctx)
	conn.Release()
}

// localSiteLocker is a siteLocker for one process. It is what a Service built
// without a database pool uses, which is only ever a unit test; production
// always has the pool and uses pgSiteLocker.
type localSiteLocker struct {
	wait time.Duration
	mu   sync.Mutex
	held map[uuid.UUID]chan struct{}
}

func newLocalSiteLocker(wait time.Duration) *localSiteLocker {
	return &localSiteLocker{wait: wait, held: map[uuid.UUID]chan struct{}{}}
}

func (l *localSiteLocker) lockSite(ctx context.Context, siteID uuid.UUID) (func(), error) {
	timer := time.NewTimer(l.wait)
	defer timer.Stop()
	for {
		l.mu.Lock()
		busy, isHeld := l.held[siteID]
		if !isHeld {
			done := make(chan struct{})
			l.held[siteID] = done
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					l.mu.Lock()
					delete(l.held, siteID)
					l.mu.Unlock()
					close(done)
				})
			}, nil
		}
		l.mu.Unlock()
		select {
		case <-busy:
		case <-timer.C:
			return nil, errSiteEmailBusy
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
