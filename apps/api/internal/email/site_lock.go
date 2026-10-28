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
// pinned connection, so it excludes across every API instance. That connection
// stays out of the pool for the whole operation, agent calls included, so each
// instance admits only a few such operations at once: see emailSlots.
//
// Without it a push of the row as it already stood, in flight when a removal
// runs, lands on the site after the removal cleared it: the dashboard says the
// provider is gone while the site still holds it and its credential.
const SiteEmailLockKey = "email_site_config"

// siteLockWait bounds how long an operation waits to start, for a slot on
// this instance and for the site's lock together. A removal holds the lock
// across two agent calls, each bounded by lockedAgentCallTimeout.
const siteLockWait = 10 * time.Second

// siteLockPoll is how often a waiting operation tries again. Between tries it
// holds neither a slot nor a pooled connection.
const siteLockPoll = 100 * time.Millisecond

// instanceEmailCap is how many operations holding a site's email lock one
// instance runs at once, across every tenant. Each keeps a pooled connection
// for its whole length, and the pool holds five (db.ConnectApp), so two
// always stay free for every other request on the instance.
const instanceEmailCap = 3

// lockedAgentCallTimeout bounds one agent call made while the site's email
// lock is held: the removal's clear and its follow-up push, a save's push,
// Sync to site, and the send-test pre-sync. The lock, its slot and its
// connection are held for as long as the call runs, so a slow site is cut
// off well inside the shared client's own limit, and a removal holds the lock
// for at most two such calls.
const lockedAgentCallTimeout = 15 * time.Second

// instanceEmailSlots is this process's admission for operations that hold a
// site's email lock: one per tenant, instanceEmailCap in all.
var instanceEmailSlots = newEmailSlots(instanceEmailCap)

// emailSlots admits operations that hold a site's email lock on one instance:
// at most one per tenant, so a tenant's slow site holds back only that
// tenant's own email changes here, and at most max in all, so the pinned
// connections never take the whole pool. A slot is taken without waiting and
// is held only while its operation holds, or is in one try for, the site's
// lock.
type emailSlots struct {
	mu      sync.Mutex
	max     int
	tenants map[uuid.UUID]struct{}
}

func newEmailSlots(max int) *emailSlots {
	return &emailSlots{max: max, tenants: map[uuid.UUID]struct{}{}}
}

// tryTake takes tenantID's slot when the tenant holds none here and the
// instance has one free, and reports whether it did. It never waits.
func (s *emailSlots) tryTake(tenantID uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.tenants[tenantID]; held || len(s.tenants) >= s.max {
		return false
	}
	s.tenants[tenantID] = struct{}{}
	return true
}

// give hands tenantID's slot back.
func (s *emailSlots) give(tenantID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tenants, tenantID)
}

// inUse is how many slots are taken.
func (s *emailSlots) inUse() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tenants)
}

// errSiteEmailBusy is returned by a siteLocker when the wait ran out.
var errSiteEmailBusy = errors.New("email: another email change is in progress")

// siteBusyMessage is the operator's answer when the wait ran out, for this
// site's lock or for a slot on this instance. Nothing was changed.
const siteBusyMessage = "Another email change is in progress. Try again in a moment."

// siteBusyError is the 409 for a wait that ran out.
func siteBusyError() *domain.Error {
	return domain.Conflict("email_change_in_progress", siteBusyMessage)
}

// siteLocker takes the per-site email lock.
type siteLocker interface {
	// lockSite returns once the site's lock is held, errSiteEmailBusy when the
	// bounded wait ran out, or the error that stopped it. tenantID is the
	// site's tenant. release must be called exactly once, on every exit;
	// extra calls are no-ops.
	lockSite(ctx context.Context, tenantID, siteID uuid.UUID) (release func(), err error)
}

// lockSiteForChange takes the site's lock and turns a failure into the domain
// error a caller returns as is: 409 when the wait ran out, 503 otherwise. In
// both cases nothing has been changed. The error is returned as the concrete
// type so a caller can read its Message; callers return it only when non-nil.
func (s *Service) lockSiteForChange(ctx context.Context, tenantID, siteID uuid.UUID) (func(), *domain.Error) {
	release, err := s.locks.lockSite(ctx, tenantID, siteID)
	if err == nil {
		return release, nil
	}
	if errors.Is(err, errSiteEmailBusy) {
		return nil, siteBusyError()
	}
	return nil, domain.ServiceUnavailable("email_site_lock_failed",
		"Could not start the email change for this site; nothing was changed. Try again.").WithCause(err)
}

// lockedAgentCtx bounds one agent call made under the site's email lock by
// lockedAgentCallTimeout, or by the Service's override in a test.
func (s *Service) lockedAgentCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	d := s.lockedCallTimeout
	if d <= 0 {
		d = lockedAgentCallTimeout
	}
	return context.WithTimeout(ctx, d)
}

// pgSiteLocker is the production siteLocker: a session-level Postgres advisory
// lock held on one pinned pooled connection for the whole operation. A
// session lock, not a transaction lock, because the operation spans agent
// calls, and a transaction left open across them is cut by the server's
// idle-in-transaction timeout.
type pgSiteLocker struct {
	pool *db.Pool
	// slots is the instance's admission, instanceEmailSlots in production. A
	// slot is taken before a connection is and given back after it is.
	slots *emailSlots
	wait  time.Duration
	poll  time.Duration
}

// lockSite tries, every poll until the wait runs out: a slot without waiting,
// then a connection, then the site's lock. A try that does not get the lock
// gives the connection and the slot back before it sleeps, so a waiter holds
// neither, and a site locked from another instance holds back nothing here.
func (l *pgSiteLocker) lockSite(ctx context.Context, tenantID, siteID uuid.UUID) (func(), error) {
	deadline := time.Now().Add(l.wait)
	for {
		conn, err := l.tryLock(ctx, tenantID, siteID, deadline)
		if err != nil {
			return nil, err
		}
		if conn != nil {
			var once sync.Once
			return func() {
				once.Do(func() {
					defer l.slots.give(tenantID)
					unlockSiteConn(ctx, conn, siteID)
				})
			}, nil
		}

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

// tryLock makes one try. It returns the pinned connection when the site's
// lock is held, or nil and no error when the slot or the lock was taken, with
// everything it took given back. It waits for a connection until deadline at
// most.
func (l *pgSiteLocker) tryLock(ctx context.Context, tenantID, siteID uuid.UUID, deadline time.Time) (*pgxpool.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !l.slots.tryTake(tenantID) {
		return nil, nil
	}
	locked := false
	defer func() {
		if !locked {
			l.slots.give(tenantID)
		}
	}()
	actx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	conn, err := l.pool.Acquire(actx)
	if err != nil {
		return nil, err
	}
	var got bool
	if err := conn.QueryRow(actx,
		`SELECT pg_try_advisory_lock(hashtext($1), hashtext($2))`,
		SiteEmailLockKey, siteID.String(),
	).Scan(&got); err != nil {
		// The server may have taken the lock before the reply was lost, so
		// the session is closed rather than pooled: ending it drops every
		// lock it holds.
		discardLockConn(ctx, conn)
		return nil, err
	}
	if !got {
		conn.Release()
		return nil, nil
	}
	locked = true
	return conn, nil
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
// always has the pool and uses pgSiteLocker. It holds no connection, so it
// takes no slot, and it excludes per site only.
type localSiteLocker struct {
	wait time.Duration
	mu   sync.Mutex
	held map[uuid.UUID]chan struct{}
}

func newLocalSiteLocker(wait time.Duration) *localSiteLocker {
	return &localSiteLocker{wait: wait, held: map[uuid.UUID]chan struct{}{}}
}

func (l *localSiteLocker) lockSite(ctx context.Context, _, siteID uuid.UUID) (func(), error) {
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
