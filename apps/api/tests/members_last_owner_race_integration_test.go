package tests

// members_last_owner_race_integration_test.go — GH #406 follow-up: the
// last-owner guard was CHECK-THEN-ACT ACROSS SEPARATE TRANSACTIONS.
//
// members_handler.patchRole read the target membership (one InTenantTx),
// counted owners (a second), then wrote the new role (a third). Two concurrent
// demotes of two DIFFERENT owners therefore both read ownerCount=2, both passed
// `ownerCount <= 1`, and both wrote: the organisation ended with ZERO owners.
// removeMember had the same shape and lost the same way.
//
// The race predates 702ed45. Its CONSEQUENCE does not. Before 702ed45 a
// zero-owner org could repair itself from inside the product — an admin minted
// an owner-role API key, or promoted themselves. 702ed45 closed both of those
// doors, because they WERE the privilege escalation. So after 702ed45 a
// zero-owner org is unrecoverable without direct database access, and every
// owner-only capability (tenant:manage, smtp:manage, billing:manage,
// audit:manage, org delete/restore) is permanently dead for that tenant.
//
// WHAT THIS FILE PINS
//   - Concurrent demotes can never leave zero owners (2-owner and 3-owner orgs).
//   - Concurrent removes can never leave zero owners.
//   - SEQUENTIAL behaviour is UNCHANGED: the second demote still 403s with code
//     "last_owner". A fix that makes BOTH demotes fail has broken the feature,
//     not fixed the race — that assertion is what stops the guard being
//     "fixed" by refusing everything.
//
// Everything under test goes through the REAL gin engine and the REAL
// middleware.Authenticate -> RequireAuth/RequireTenant/RequirePermission chain,
// over HTTP, against the pool startPostgres returns: wpmgr_app, NOSUPERUSER,
// NOBYPASSRLS. The guard's locking read runs under InTenantTx, so it is
// governed by memberships_tenant_isolation — declared with no FOR clause, i.e.
// FOR ALL with the tenant predicate in USING and WITH CHECK, which is what
// makes SELECT ... FOR UPDATE return rows here at all.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane). Run with
// `make test-integration` from the repository root.

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// raceRounds is the number of independent rounds each concurrent scenario
// runs. A single round proves nothing: the reviewer's measurement of the
// pre-fix code reached zero owners in 24 of 25 PATCH rounds and 25 of 25
// DELETE rounds, so one round has a real chance of passing on broken code.
const raceRounds = 25

// raceRoundsThreeOwner is lower only because each round costs three concurrent
// requests plus three seeded users; the pre-fix code still lost 9-10 of 10.
const raceRoundsThreeOwner = 10

// raceHarness is the shared per-test fixture: one container, one engine, many
// short-lived orgs.
type raceHarness struct {
	pool    *db.Pool
	admin   *db.Pool
	repo    *auth.Repo
	svc     *auth.Service
	session *auth.SessionManager
	engine  *gin.Engine
}

func newRaceHarness(t *testing.T) *raceHarness {
	t.Helper()
	pool := startPostgres(t) // wpmgr_app: NOSUPERUSER, NOBYPASSRLS
	gh406RequireApplicationRole(t, pool)
	adminDB := connectAdmin(t, pool)
	t.Cleanup(adminDB.Close)

	rec := audit.NewRecorder(pool, domain.SystemClock{})
	repo := auth.NewRepo(pool)
	svc := auth.NewService(repo, rec, domain.NewValidator())
	sessions := auth.NewSessionManagerWithStore(scs.New(), false)

	return &raceHarness{
		pool:    pool,
		admin:   adminDB,
		repo:    repo,
		svc:     svc,
		session: sessions,
		engine:  gh406Engine(t, pool, sessions, svc, rec),
	}
}

// seedOwners creates a fresh tenant with n owner memberships and returns them.
func (h *raceHarness) seedOwners(t *testing.T, n int) (uuid.UUID, []auth.User) {
	t.Helper()
	sfx := uuid.NewString()[:8]
	tenant := seedTenant(t, h.pool, "race-"+sfx)
	owners := make([]auth.User, 0, n)
	for i := 0; i < n; i++ {
		owners = append(owners, seedUserMembership(t,
			h.repo, "race-owner"+string(rune('a'+i))+"-"+sfx+"@example.com", tenant, authz.RoleOwner))
	}
	// PREMISE, asserted through the production path, not assumed: with fewer
	// owners than we think, the last-owner guard supplies a 403 for reasons
	// that have nothing to do with the race and every assertion goes vacuous.
	got, err := h.svc.CountOwners(context.Background(), tenant)
	if err != nil {
		t.Fatalf("count owners: %v", err)
	}
	if got != n {
		t.Fatalf("PREMISE FAILED: seeded org holds %d owners, want %d", got, n)
	}
	return tenant, owners
}

// ownersInDB reads the surviving owner count out of band (superuser pool), so
// the assertion does not depend on the code path under test.
func (h *raceHarness) ownersInDB(t *testing.T, tenant uuid.UUID) int {
	t.Helper()
	var n int
	if err := h.admin.QueryRow(context.Background(),
		`SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'owner'`, tenant).Scan(&n); err != nil {
		t.Fatalf("count owners out of band: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// The race
// ---------------------------------------------------------------------------

// TestGH406Race_ConcurrentDemotesCannotReachZeroOwners fires N owners' demotes
// of each OTHER at the same instant. Every actor is an owner and every target
// is an owner, so the 702ed45 target-role ceiling permits all of them
// (owner-on-owner is a supported ownership transfer) — the ONLY thing standing
// between this and a zero-owner org is the last-owner guard being atomic.
func TestGH406Race_ConcurrentDemotesCannotReachZeroOwners(t *testing.T) {
	h := newRaceHarness(t)

	t.Run("two_owners", func(t *testing.T) {
		zero := 0
		for round := 0; round < raceRounds; round++ {
			tenant, owners := h.seedOwners(t, 2)
			// owners[0] demotes owners[1] while owners[1] demotes owners[0].
			h.fireConcurrent(t, tenant, owners, http.MethodPatch)
			if n := h.ownersInDB(t, tenant); n == 0 {
				zero++
			}
		}
		if zero > 0 {
			t.Fatalf("concurrent demotes reached ZERO owners in %d of %d rounds: the last-owner "+
				"guard is check-then-act across transactions and the org is now unrecoverable "+
				"without direct database access", zero, raceRounds)
		}
		t.Logf("2-owner org, 2 concurrent demotes: zero-owner outcomes %d/%d", zero, raceRounds)
	})

	t.Run("three_owners", func(t *testing.T) {
		zero := 0
		for round := 0; round < raceRoundsThreeOwner; round++ {
			tenant, owners := h.seedOwners(t, 3)
			h.fireConcurrent(t, tenant, owners, http.MethodPatch)
			if n := h.ownersInDB(t, tenant); n == 0 {
				zero++
			}
		}
		if zero > 0 {
			t.Fatalf("3-owner org, 3 concurrent demotes reached ZERO owners in %d of %d rounds",
				zero, raceRoundsThreeOwner)
		}
		t.Logf("3-owner org, 3 concurrent demotes: zero-owner outcomes %d/%d", zero, raceRoundsThreeOwner)
	})
}

// TestGH406Race_ConcurrentRemovesCannotReachZeroOwners is the DELETE half. The
// reviewer measured this one losing 25 rounds out of 25.
func TestGH406Race_ConcurrentRemovesCannotReachZeroOwners(t *testing.T) {
	h := newRaceHarness(t)

	zero := 0
	for round := 0; round < raceRounds; round++ {
		tenant, owners := h.seedOwners(t, 2)
		h.fireConcurrent(t, tenant, owners, http.MethodDelete)
		if n := h.ownersInDB(t, tenant); n == 0 {
			zero++
		}
	}
	if zero > 0 {
		t.Fatalf("concurrent removes reached ZERO owners in %d of %d rounds", zero, raceRounds)
	}
	t.Logf("2-owner org, 2 concurrent removes: zero-owner outcomes %d/%d", zero, raceRounds)
}

// fireConcurrent has owner i act on owner i+1 (wrapping), all released at once
// by a single starting gate. method is PATCH (demote to admin) or DELETE.
func (h *raceHarness) fireConcurrent(t *testing.T, tenant uuid.UUID, owners []auth.User, method string) {
	t.Helper()
	n := len(owners)
	// Sessions are built BEFORE the gate: session creation is itself a DB
	// write, and doing it inside the goroutines would stagger the requests
	// enough to hide the race.
	ctxs := make([]context.Context, n)
	for i := range owners {
		ctxs[i] = gh406Session(t, h.session, owners[i].ID, tenant)
	}

	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := owners[(i+1)%n].ID
			<-gate
			if method == http.MethodDelete {
				gh406Do(h.engine, ctxs[i], http.MethodDelete, "/api/v1/members/"+target.String(), "")
				return
			}
			gh406Do(h.engine, ctxs[i], http.MethodPatch,
				"/api/v1/members/"+target.String(), `{"role":"admin"}`)
		}(i)
	}
	close(gate)
	wg.Wait()
}

// ---------------------------------------------------------------------------
// The feature the fix must NOT break
// ---------------------------------------------------------------------------

// TestGH406Race_SequentialDemoteStill403s pins the behaviour a "fix" that
// simply refuses everything would destroy: run one at a time, the FIRST demote
// of an owner succeeds and the SECOND is refused with code "last_owner".
func TestGH406Race_SequentialDemoteStill403s(t *testing.T) {
	h := newRaceHarness(t)
	tenant, owners := h.seedOwners(t, 2)

	ctxA := gh406Session(t, h.session, owners[0].ID, tenant)

	// First demote: owners[0] demotes owners[1]. Two owners exist, so this is
	// a legitimate ownership change and must succeed.
	w := gh406Do(h.engine, ctxA, http.MethodPatch,
		"/api/v1/members/"+owners[1].ID.String(), `{"role":"admin"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("first demote: got %d (%s), want 200 — the fix has broken legitimate "+
			"ownership changes, not just the race", w.Code, w.Body.String())
	}
	if role := gh406RoleInDB(t, h.admin, tenant, owners[1].ID); role != "admin" {
		t.Fatalf("first demote: stored role is %q, want \"admin\"", role)
	}

	// Second demote: owners[0] demotes THEMSELVES, now the last owner. Refused,
	// and the refusal is actor-independent — this is an OWNER being told no.
	w = gh406Do(h.engine, ctxA, http.MethodPatch,
		"/api/v1/members/"+owners[0].ID.String(), `{"role":"admin"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("second demote: got %d (%s), want 403", w.Code, w.Body.String())
	}
	if code := gh406Code(t, w); code != "last_owner" {
		t.Fatalf("second demote: error code %q, want \"last_owner\"", code)
	}
	if role := gh406RoleInDB(t, h.admin, tenant, owners[0].ID); role != "owner" {
		t.Fatalf("second demote: 403 returned but stored role is %q, want \"owner\" — "+
			"a refusal that still writes the row is not a refusal", role)
	}
	if n := h.ownersInDB(t, tenant); n != 1 {
		t.Fatalf("sequential path left %d owners, want 1", n)
	}
	t.Logf("sequential: first demote 200, second demote 403 last_owner, %d owner survives", 1)
}

// TestGH406Race_SequentialRemoveStill403s is the DELETE half of the above.
func TestGH406Race_SequentialRemoveStill403s(t *testing.T) {
	h := newRaceHarness(t)
	tenant, owners := h.seedOwners(t, 2)

	ctxA := gh406Session(t, h.session, owners[0].ID, tenant)

	w := gh406Do(h.engine, ctxA, http.MethodDelete,
		"/api/v1/members/"+owners[1].ID.String(), "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("first remove: got %d (%s), want 204", w.Code, w.Body.String())
	}
	if gh406MembershipExists(t, h.admin, tenant, owners[1].ID) {
		t.Fatalf("first remove returned 204 but the membership row survives")
	}

	w = gh406Do(h.engine, ctxA, http.MethodDelete,
		"/api/v1/members/"+owners[0].ID.String(), "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("second remove: got %d (%s), want 403", w.Code, w.Body.String())
	}
	if code := gh406Code(t, w); code != "last_owner" {
		t.Fatalf("second remove: error code %q, want \"last_owner\"", code)
	}
	if !gh406MembershipExists(t, h.admin, tenant, owners[0].ID) {
		t.Fatalf("second remove: 403 returned but the last owner's membership was deleted")
	}
	if n := h.ownersInDB(t, tenant); n != 1 {
		t.Fatalf("sequential remove path left %d owners, want 1", n)
	}
}
