package aireadiness

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

type loadCall struct {
	tenant uuid.UUID
	site   *uuid.UUID
}

// fakeRepo stands in for the row policies: a principal reads only the rows of
// its own tenant, exactly as the tenant transaction lets it.
type fakeRepo struct {
	facts    []Facts
	tenantOf map[uuid.UUID]uuid.UUID
	targets  map[uuid.UUID]RefreshTarget

	loadErr   error
	targetErr error

	loads       []loadCall
	targetReads []uuid.UUID
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{tenantOf: map[uuid.UUID]uuid.UUID{}, targets: map[uuid.UUID]RefreshTarget{}}
}

func (r *fakeRepo) add(tenant uuid.UUID, f Facts) uuid.UUID {
	r.facts = append(r.facts, f)
	r.tenantOf[f.SiteID] = tenant
	return f.SiteID
}

func (r *fakeRepo) LoadFacts(_ context.Context, p domain.Principal, siteID *uuid.UUID) ([]Facts, error) {
	r.loads = append(r.loads, loadCall{tenant: p.TenantID, site: siteID})
	if r.loadErr != nil {
		return nil, r.loadErr
	}
	var out []Facts
	for _, f := range r.facts {
		if r.tenantOf[f.SiteID] != p.TenantID {
			continue
		}
		if siteID != nil && f.SiteID != *siteID {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

func (r *fakeRepo) RefreshTarget(_ context.Context, p domain.Principal, siteID uuid.UUID) (RefreshTarget, error) {
	r.targetReads = append(r.targetReads, siteID)
	if r.targetErr != nil {
		return RefreshTarget{}, r.targetErr
	}
	t, ok := r.targets[siteID]
	if !ok || r.tenantOf[siteID] != p.TenantID {
		return RefreshTarget{}, domain.NotFound("site_not_found", "site not found")
	}
	return t, nil
}

// strayRepo ignores what it is asked and returns fixed rows.
type strayRepo struct{ rows []Facts }

func (r strayRepo) LoadFacts(context.Context, domain.Principal, *uuid.UUID) ([]Facts, error) {
	return r.rows, nil
}

func (r strayRepo) RefreshTarget(context.Context, domain.Principal, uuid.UUID) (RefreshTarget, error) {
	return RefreshTarget{}, domain.NotFound("site_not_found", "site not found")
}

type metaCall struct {
	tenant, site uuid.UUID
	url, source  string
}

type fakeMeta struct {
	calls []metaCall
	err   error
}

func (m *fakeMeta) EnqueueRefresh(_ context.Context, tenantID, siteID uuid.UUID, siteURL, source string) error {
	m.calls = append(m.calls, metaCall{tenantID, siteID, siteURL, source})
	return m.err
}

type invCall struct{ tenant, site uuid.UUID }

type fakeInventory struct {
	calls  []invCall
	queued bool
	err    error
}

func (i *fakeInventory) EnqueueInventoryRefresh(_ context.Context, tenantID, siteID uuid.UUID) (bool, error) {
	i.calls = append(i.calls, invCall{tenantID, siteID})
	return i.queued, i.err
}

type fakeAudit struct {
	events []audit.Event
	err    error
}

func (a *fakeAudit) Record(_ context.Context, e audit.Event) (audit.Entry, error) {
	a.events = append(a.events, e)
	return audit.Entry{}, a.err
}

func orgPrincipal(tenant uuid.UUID) domain.Principal {
	return domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Role: "viewer", Scope: domain.ScopeOrg}
}

func sitePrincipal(tenant uuid.UUID, sites ...uuid.UUID) domain.Principal {
	return domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Role: "operator", Scope: domain.ScopeSite, AllowedSiteIDs: sites}
}

func wantDomainError(t *testing.T, err error, status int, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want %d %s", status, code)
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("got %T %v, want a domain error %s", err, err, code)
	}
	if de.Code != code || domain.HTTPStatus(err) != status {
		t.Fatalf("got %d %s (%s), want %d %s", domain.HTTPStatus(err), de.Code, de.Message, status, code)
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)) }

func newService(repo Repo) *Service { return NewService(repo, nil, quietLogger()) }

// ---- Get ---------------------------------------------------------------------

func TestGetEvaluatesTheNamedSite(t *testing.T) {
	tenant := uuid.New()
	repo := newFakeRepo()
	other := readyFacts()
	broken := readyFacts()
	broken.ContentEditingEnabled = false
	repo.add(tenant, other)
	id := repo.add(tenant, broken)

	res, err := newService(repo).Get(context.Background(), orgPrincipal(tenant), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.SiteID != id || res.Status != StatusNeedsAttention || res.FixCount != 1 {
		t.Fatalf("got site %s status %q fix_count %d, want site %s needs_attention 1", res.SiteID, res.Status, res.FixCount, id)
	}
	if len(repo.loads) != 1 || repo.loads[0].tenant != tenant || repo.loads[0].site == nil || *repo.loads[0].site != id {
		t.Fatalf("the repo must be asked for exactly this site in the caller's tenant: %+v", repo.loads)
	}
}

func TestGetIsNotFoundForAnotherTenantsSite(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	repo := newFakeRepo()
	siteA := repo.add(tenantA, readyFacts())
	svc := newService(repo)

	_, err := svc.Get(context.Background(), orgPrincipal(tenantB), siteA)
	wantDomainError(t, err, http.StatusNotFound, "site_not_found")

	_, errMissing := svc.Get(context.Background(), orgPrincipal(tenantB), uuid.New())
	wantDomainError(t, errMissing, http.StatusNotFound, "site_not_found")
	if err.Error() != errMissing.Error() {
		t.Fatalf("another tenant's site and a site that does not exist must be indistinguishable: %q vs %q", err, errMissing)
	}

	res, err := svc.Get(context.Background(), orgPrincipal(tenantA), siteA)
	if err != nil || res.SiteID != siteA {
		t.Fatalf("OVER-FIRING: the owning tenant must still read its site: %+v %v", res, err)
	}
}

func TestGetIsNotFoundOutsideTheCallersSiteAllowlist(t *testing.T) {
	tenant := uuid.New()
	repo := newFakeRepo()
	allowed := repo.add(tenant, readyFacts())
	denied := repo.add(tenant, readyFacts())
	svc := newService(repo)

	_, err := svc.Get(context.Background(), sitePrincipal(tenant, allowed), denied)
	wantDomainError(t, err, http.StatusNotFound, "site_not_found")
	if len(repo.loads) != 0 {
		t.Fatalf("a site outside the allowlist must be refused before any read: %+v", repo.loads)
	}
	if _, err := svc.Get(context.Background(), sitePrincipal(tenant, allowed), allowed); err != nil {
		t.Fatalf("OVER-FIRING: the allowed site must be readable: %v", err)
	}
	_, err = svc.Get(context.Background(), sitePrincipal(tenant), allowed)
	wantDomainError(t, err, http.StatusNotFound, "site_not_found")
}

func TestGetNeverAnswersWithAnotherSitesRow(t *testing.T) {
	tenant := uuid.New()
	stray := readyFacts()
	svc := newService(strayRepo{rows: []Facts{stray}})
	_, err := svc.Get(context.Background(), orgPrincipal(tenant), uuid.New())
	wantDomainError(t, err, http.StatusNotFound, "site_not_found")
}

func TestGetAndFleetMapAReadFailureToAnInternalError(t *testing.T) {
	repo := newFakeRepo()
	repo.loadErr = errors.New("connection reset")
	svc := newService(repo)
	p := orgPrincipal(uuid.New())

	for name, call := range map[string]func() error{
		"Get":   func() error { _, err := svc.Get(context.Background(), p, uuid.New()); return err },
		"Fleet": func() error { _, err := svc.Fleet(context.Background(), p); return err },
	} {
		err := call()
		wantDomainError(t, err, http.StatusInternalServerError, "ai_readiness_read_failed")
		if !errors.Is(err, repo.loadErr) {
			t.Errorf("%s: the cause must travel with the error: %v", name, err)
		}
		if de, _ := domain.AsDomain(err); strings.Contains(de.Message, "connection reset") {
			t.Errorf("%s: the infrastructure error must not become the message a caller sees: %q", name, de.Message)
		}
	}
}

// ---- Fleet -------------------------------------------------------------------

func TestFleetFiltersWithCanAccessSite(t *testing.T) {
	tenant := uuid.New()
	repo := newFakeRepo()
	s1 := repo.add(tenant, readyFacts())
	s2 := repo.add(tenant, readyFacts())
	s3 := repo.add(tenant, readyFacts())
	svc := newService(repo)

	ids := func(rs []Result) []uuid.UUID {
		out := make([]uuid.UUID, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.SiteID)
		}
		return out
	}
	same := func(got, want []uuid.UUID) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	all, err := svc.Fleet(context.Background(), orgPrincipal(tenant))
	if err != nil || !same(ids(all), []uuid.UUID{s1, s2, s3}) {
		t.Fatalf("OVER-FIRING: an org member must see every site of the tenant in order: %v %v", ids(all), err)
	}

	one, err := svc.Fleet(context.Background(), sitePrincipal(tenant, s2))
	if err != nil || !same(ids(one), []uuid.UUID{s2}) {
		t.Fatalf("SITE-SCOPE LEAK: a collaborator allowed only %s got %v (%v)", s2, ids(one), err)
	}

	two, err := svc.Fleet(context.Background(), sitePrincipal(tenant, s3, s1))
	if err != nil || !same(ids(two), []uuid.UUID{s1, s3}) {
		t.Fatalf("SITE-SCOPE LEAK: a collaborator allowed %s and %s got %v (%v)", s3, s1, ids(two), err)
	}

	none, err := svc.Fleet(context.Background(), sitePrincipal(tenant))
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("SITE-SCOPE LEAK: a collaborator with an empty allowlist must get an empty, non-nil list: %#v %v", none, err)
	}

	for _, call := range repo.loads {
		if call.site != nil {
			t.Fatalf("the fleet read must not name a site: %+v", call)
		}
	}
}

func TestFleetReadsOnlyTheCallersTenant(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	repo := newFakeRepo()
	repo.add(tenantA, readyFacts())
	siteB := repo.add(tenantB, readyFacts())

	got, err := newService(repo).Fleet(context.Background(), orgPrincipal(tenantB))
	if err != nil || len(got) != 1 || got[0].SiteID != siteB {
		t.Fatalf("TENANCY LEAK: tenant B's fleet = %+v (%v), want only %s", got, err, siteB)
	}
	if len(repo.loads) != 1 || repo.loads[0].tenant != tenantB {
		t.Fatalf("the read must carry the caller's tenant: %+v", repo.loads)
	}
}

func TestFleetEvaluatesEachSiteOnItsOwnFacts(t *testing.T) {
	tenant := uuid.New()
	repo := newFakeRepo()
	repo.add(tenant, readyFacts())
	bad := readyFacts()
	bad.WPVersion = "6.0"
	bad.ContentEditingEnabled = false
	badID := repo.add(tenant, bad)

	got, err := newService(repo).Fleet(context.Background(), orgPrincipal(tenant))
	if err != nil || len(got) != 2 {
		t.Fatalf("Fleet: %+v %v", got, err)
	}
	byID := map[uuid.UUID]Result{got[0].SiteID: got[0], got[1].SiteID: got[1]}
	if r := byID[badID]; r.Status != StatusNeedsAttention || r.FixCount != 2 {
		t.Fatalf("the broken site = %q %d, want needs_attention 2", r.Status, r.FixCount)
	}
	for id, r := range byID {
		if id != badID && (r.Status != StatusReady || r.FixCount != 0) {
			t.Fatalf("the healthy site = %q %d, want ready 0", r.Status, r.FixCount)
		}
	}
}

// ---- RequestRefresh ------------------------------------------------------------

type refreshRig struct {
	tenant uuid.UUID
	site   uuid.UUID
	now    time.Time
	repo   *fakeRepo
	meta   *fakeMeta
	inv    *fakeInventory
	audit  *fakeAudit
	logs   *bytes.Buffer
	svc    *Service
	target RefreshTarget
	stale  time.Duration
}

func newRefreshRig(t *testing.T) *refreshRig {
	t.Helper()
	rig := &refreshRig{
		tenant: uuid.New(),
		now:    time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
		repo:   newFakeRepo(),
		meta:   &fakeMeta{},
		inv:    &fakeInventory{queued: true},
		audit:  &fakeAudit{},
		logs:   &bytes.Buffer{},
		stale:  10 * time.Minute,
	}
	rig.site = rig.repo.add(rig.tenant, readyFacts())
	seen := rig.now.Add(-time.Minute)
	rig.target = RefreshTarget{
		SiteID: rig.site, URL: "https://site.example.test", Enrolled: true,
		LastSeenAt: &seen, AgentVersion: DefaultFloors().EngineAgent, ConnectionState: "connected",
	}
	return rig
}

func (r *refreshRig) build() *Service {
	r.repo.targets[r.site] = r.target
	svc := NewService(r.repo, nil, slog.New(slog.NewTextHandler(r.logs, nil)))
	svc.SetAuditRecorder(r.audit)
	svc.SetClock(func() time.Time { return r.now })
	svc.SetRefreshers(r.meta, r.inv, r.stale)
	r.svc = svc
	return svc
}

func (r *refreshRig) nothingQueued(t *testing.T) {
	t.Helper()
	if len(r.meta.calls) != 0 || len(r.inv.calls) != 0 || len(r.audit.events) != 0 {
		t.Fatalf("nothing may be queued or audited: meta %+v inventory %+v audit %+v", r.meta.calls, r.inv.calls, r.audit.events)
	}
}

func TestRefreshQueuesBothReadsForACurrentAgent(t *testing.T) {
	rig := newRefreshRig(t)
	res, err := rig.build().RequestRefresh(context.Background(), orgPrincipal(rig.tenant), rig.site)
	if err != nil {
		t.Fatalf("RequestRefresh: %v", err)
	}
	if !res.Metadata || !res.Abilities {
		t.Fatalf("got %+v, want both reads queued", res)
	}
	if len(rig.meta.calls) != 1 || rig.meta.calls[0] != (metaCall{rig.tenant, rig.site, "https://site.example.test", refreshSource}) {
		t.Fatalf("metadata refresh = %+v", rig.meta.calls)
	}
	if len(rig.inv.calls) != 1 || rig.inv.calls[0] != (invCall{rig.tenant, rig.site}) {
		t.Fatalf("inventory refresh = %+v", rig.inv.calls)
	}
	if len(rig.audit.events) != 1 {
		t.Fatalf("audit events = %+v, want one", rig.audit.events)
	}
	ev := rig.audit.events[0]
	if ev.Action != audit.ActionAIReadinessRefreshRequested || ev.TenantID != rig.tenant ||
		ev.TargetType != "site" || ev.TargetID != rig.site.String() || ev.Metadata["abilities"] != true {
		t.Fatalf("audit event = %+v", ev)
	}
}

func TestRefreshIsConflictWhenTheSiteIsNotEnrolled(t *testing.T) {
	rig := newRefreshRig(t)
	rig.target.Enrolled = false
	_, err := rig.build().RequestRefresh(context.Background(), orgPrincipal(rig.tenant), rig.site)
	wantDomainError(t, err, http.StatusConflict, "site_unreachable")
	rig.nothingQueued(t)
}

func TestRefreshIsConflictWhenTheAgentHeartbeatIsStale(t *testing.T) {
	cases := []struct {
		name     string
		lastSeen func(now time.Time, stale time.Duration) *time.Time
		stale    time.Duration
		conflict bool
	}{
		{"never heard from", func(time.Time, time.Duration) *time.Time { return nil }, 10 * time.Minute, true},
		{"silent for an hour", func(now time.Time, _ time.Duration) *time.Time { v := now.Add(-time.Hour); return &v }, 10 * time.Minute, true},
		{"one nanosecond past the window", func(now time.Time, s time.Duration) *time.Time { v := now.Add(-s - time.Nanosecond); return &v }, 10 * time.Minute, true},
		{"exactly at the window", func(now time.Time, s time.Duration) *time.Time { v := now.Add(-s); return &v }, 10 * time.Minute, false},
		{"just seen", func(now time.Time, _ time.Duration) *time.Time { v := now.Add(-time.Second); return &v }, 10 * time.Minute, false},
		{"the gate is off: never heard from", func(time.Time, time.Duration) *time.Time { return nil }, 0, false},
		{"the gate is off: silent for a day", func(now time.Time, _ time.Duration) *time.Time { v := now.Add(-24 * time.Hour); return &v }, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig := newRefreshRig(t)
			rig.stale = c.stale
			rig.target.LastSeenAt = c.lastSeen(rig.now, c.stale)
			_, err := rig.build().RequestRefresh(context.Background(), orgPrincipal(rig.tenant), rig.site)
			if c.conflict {
				wantDomainError(t, err, http.StatusConflict, "site_unreachable")
				rig.nothingQueued(t)
				return
			}
			if err != nil {
				t.Fatalf("OVER-FIRING: a reachable site was refused: %v", err)
			}
			if len(rig.meta.calls) != 1 {
				t.Fatalf("metadata refresh = %+v, want one", rig.meta.calls)
			}
		})
	}
}

func TestRefreshIsNotFoundForASiteThatIsNotTheCallers(t *testing.T) {
	rig := newRefreshRig(t)
	svc := rig.build()
	other := uuid.New()

	_, err := svc.RequestRefresh(context.Background(), orgPrincipal(other), rig.site)
	wantDomainError(t, err, http.StatusNotFound, "site_not_found")
	rig.nothingQueued(t)

	_, err = svc.RequestRefresh(context.Background(), sitePrincipal(rig.tenant, uuid.New()), rig.site)
	wantDomainError(t, err, http.StatusNotFound, "site_not_found")
	if len(rig.repo.targetReads) != 1 {
		t.Fatalf("a site outside the allowlist must be refused before its row is read; reads: %v", rig.repo.targetReads)
	}
	rig.nothingQueued(t)

	if _, err := svc.RequestRefresh(context.Background(), sitePrincipal(rig.tenant, rig.site), rig.site); err != nil {
		t.Fatalf("OVER-FIRING: an allowed collaborator was refused: %v", err)
	}
}

func TestRefreshIsNotFoundForAnArchivedSite(t *testing.T) {
	rig := newRefreshRig(t)
	rig.target.ConnectionState = "archived"
	_, err := rig.build().RequestRefresh(context.Background(), orgPrincipal(rig.tenant), rig.site)
	wantDomainError(t, err, http.StatusNotFound, "site_not_found")
	rig.nothingQueued(t)
}

func TestRefreshWithoutRefreshersIsUnavailable(t *testing.T) {
	rig := newRefreshRig(t)
	svc := rig.build()
	svc.SetRefreshers(nil, nil, rig.stale)
	_, err := svc.RequestRefresh(context.Background(), orgPrincipal(rig.tenant), rig.site)
	wantDomainError(t, err, http.StatusServiceUnavailable, "ai_readiness_refresh_unavailable")
	rig.nothingQueued(t)
}

func TestRefreshMapsReadAndEnqueueFailures(t *testing.T) {
	t.Run("the site row cannot be read", func(t *testing.T) {
		rig := newRefreshRig(t)
		svc := rig.build()
		rig.repo.targetErr = errors.New("connection reset")
		_, err := svc.RequestRefresh(context.Background(), orgPrincipal(rig.tenant), rig.site)
		wantDomainError(t, err, http.StatusInternalServerError, "ai_readiness_read_failed")
		rig.nothingQueued(t)
	})
	t.Run("a domain error from the repo is passed through", func(t *testing.T) {
		rig := newRefreshRig(t)
		svc := rig.build()
		rig.repo.targetErr = domain.NotFound("site_not_found", "site not found")
		_, err := svc.RequestRefresh(context.Background(), orgPrincipal(rig.tenant), rig.site)
		wantDomainError(t, err, http.StatusNotFound, "site_not_found")
	})
	t.Run("the metadata refresh cannot be queued", func(t *testing.T) {
		rig := newRefreshRig(t)
		rig.meta.err = errors.New("queue down")
		_, err := rig.build().RequestRefresh(context.Background(), orgPrincipal(rig.tenant), rig.site)
		wantDomainError(t, err, http.StatusInternalServerError, "refresh_enqueue_failed")
		if len(rig.inv.calls) != 0 || len(rig.audit.events) != 0 {
			t.Fatalf("after a failed metadata enqueue nothing else may happen: inventory %+v audit %+v", rig.inv.calls, rig.audit.events)
		}
	})
}

func TestRefreshAbilitiesFlagIsGatedOnTheAgentCanReadAToolList(t *testing.T) {
	floor := DefaultFloors().EngineAgent
	cases := []struct {
		name      string
		agent     string
		noInv     bool
		invQueued bool
		invErr    error
		want      bool
		called    bool
	}{
		{"agent at the engine floor", floor, false, true, nil, true, true},
		{"agent above the engine floor", "0.99.0", false, true, nil, true, true},
		{"agent one patch below the engine floor", "0.61.154", false, true, nil, false, false},
		{"agent far below the engine floor", "0.61.1", false, true, nil, false, false},
		{"agent version not reported", "", false, true, nil, false, false},
		{"agent version of the wrong shape", "garbage", false, true, nil, false, false},
		{"agent version with a suffix", floor + "-beta", false, true, nil, false, false},
		{"no inventory refresher wired", floor, true, true, nil, false, false},
		{"an equal job queued within two minutes still counts as queued", floor, false, false, nil, true, true},
		{"the tool-list read cannot be queued", floor, false, false, errors.New("queue down"), false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig := newRefreshRig(t)
			rig.target.AgentVersion = c.agent
			rig.inv.queued, rig.inv.err = c.invQueued, c.invErr
			svc := rig.build()
			if c.noInv {
				svc.SetRefreshers(rig.meta, nil, rig.stale)
			}
			res, err := svc.RequestRefresh(context.Background(), orgPrincipal(rig.tenant), rig.site)
			if err != nil {
				t.Fatalf("RequestRefresh: %v", err)
			}
			if !res.Metadata {
				t.Fatalf("the metadata refresh must be queued whatever the agent version: %+v", res)
			}
			if res.Abilities != c.want {
				t.Fatalf("Abilities = %t, want %t", res.Abilities, c.want)
			}
			if got := len(rig.inv.calls) > 0; got != c.called {
				t.Fatalf("inventory refresher called = %t (%+v), want %t", got, rig.inv.calls, c.called)
			}
			if len(rig.audit.events) != 1 || rig.audit.events[0].Metadata["abilities"] != c.want {
				t.Fatalf("the audit row must record the flag the caller got: %+v", rig.audit.events)
			}
			if c.invErr != nil && !strings.Contains(rig.logs.String(), "tool-list read not queued") {
				t.Fatalf("a tool-list read that could not be queued must be logged: %q", rig.logs.String())
			}
		})
	}
}

func TestRefreshSurvivesAFailedAuditWrite(t *testing.T) {
	rig := newRefreshRig(t)
	rig.audit.err = errors.New("audit down")
	res, err := rig.build().RequestRefresh(context.Background(), orgPrincipal(rig.tenant), rig.site)
	if err != nil || !res.Metadata || !res.Abilities {
		t.Fatalf("a failed audit write must not undo a queued refresh: %+v %v", res, err)
	}
	if !strings.Contains(rig.logs.String(), "audit write failed") {
		t.Fatalf("a failed audit write must be logged: %q", rig.logs.String())
	}
}

func TestRefreshWithoutAnAuditRecorderStillQueues(t *testing.T) {
	rig := newRefreshRig(t)
	svc := rig.build()
	svc.audit = nil
	if res, err := svc.RequestRefresh(context.Background(), orgPrincipal(rig.tenant), rig.site); err != nil || !res.Metadata {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestInventoryRefreshFuncAdaptsAFunction(t *testing.T) {
	tenant, site := uuid.New(), uuid.New()
	var got invCall
	var f InventoryRefresher = InventoryRefreshFunc(func(_ context.Context, tn, st uuid.UUID) (bool, error) {
		got = invCall{tn, st}
		return true, nil
	})
	queued, err := f.EnqueueInventoryRefresh(context.Background(), tenant, site)
	if err != nil || !queued || got != (invCall{tenant, site}) {
		t.Fatalf("got queued=%t err=%v call=%+v", queued, err, got)
	}
}
