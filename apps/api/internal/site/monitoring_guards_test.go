// monitoring_guards_test.go — GH #414 phase 1. ONE TEST PER GUARD, and every
// one of them was shown red with its guard deleted before it was shown green
// with the guard restored.
//
// It exists because an adversarial review removed ten guards from this feature
// AT ONCE — the CanAccessSite gate, the dedup, the 200-cap, the reason cap,
// tenant_required, invalid_resume_at, the nullableUUID API-key path, both
// FOR UPDATE clauses and the audit metadata — and the entire suite stayed
// green. A guard nothing has ever been seen to fail on is not known to guard
// anything.
//
// These are the guards that need no database. The four that do (both
// FOR UPDATE clauses, the audit metadata, the archived/revoked refusal) are in
// tests/gh414_monitoring_guards_test.go, against a real Postgres as wpmgr_app.
package site

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// newMonitoringSvc returns a service over the in-memory fake, plus the fake, so
// a test can assert on WHAT REACHED THE REPO — the guards below are only
// meaningful if the rejected request never became a write.
func newMonitoringSvc() (*Service, *fakeRepo) {
	repo := &fakeRepo{}
	return NewService(repo, domain.NewValidator(), domain.SystemClock{}), repo
}

func orgPrincipal(tenant uuid.UUID) domain.Principal {
	return domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant,
		Role: "owner", Scope: domain.ScopeOrg,
	}
}

func scopedPrincipal(tenant uuid.UUID, allowed ...uuid.UUID) domain.Principal {
	return domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant,
		Role: "member", Scope: domain.ScopeSite, AllowedSiteIDs: allowed,
	}
}

// requireDomainCode fails unless err is a domain error carrying exactly code.
func requireDomainCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected domain error %q, got nil", code)
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error %q, got infra error: %v", code, err)
	}
	if de.Code != code {
		t.Fatalf("expected domain code %q, got %q (%v)", code, de.Code, err)
	}
}

// GUARD 1 — the CanAccessSite gate in partitionSiteIDs.
//
// Mutation: delete the `if !p.CanAccessSite(id)` branch. The ungranted site
// then reaches `authorized` and this test fails on both assertions.
func TestMonitoringGuard_CanAccessSiteRejectsUngrantedSite(t *testing.T) {
	granted, ungranted := uuid.New(), uuid.New()
	p := scopedPrincipal(uuid.New(), granted)

	rejected, authorized := partitionSiteIDs(p, []string{granted.String(), ungranted.String()})

	if len(authorized) != 1 || authorized[0] != granted {
		t.Fatalf("a site-scoped principal must only authorize its granted site, got %v", authorized)
	}
	if len(rejected) != 1 || rejected[0].SiteID != ungranted.String() || rejected[0].Detail != "forbidden" {
		t.Fatalf("the ungranted site must come back forbidden, got %+v", rejected)
	}
	if rejected[0].OK {
		t.Fatalf("a forbidden result must never be ok:true, got %+v", rejected[0])
	}
}

// An org-scoped principal is tenant-wide and must NOT be filtered by this gate
// — the honest case the guard must not block. Without it, a fix that returned
// false unconditionally would still pass the test above.
func TestMonitoringGuard_OrgPrincipalIsNotSiteFiltered(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	rejected, authorized := partitionSiteIDs(orgPrincipal(uuid.New()), []string{a.String(), b.String()})
	if len(authorized) != 2 {
		t.Fatalf("an org-scoped principal must authorize every named site, got %v (rejected %+v)", authorized, rejected)
	}
}

// GUARD 2 — the dedup in partitionSiteIDs.
//
// Mutation: delete the `if seen[id] { continue }` branch (and its seenInvalid
// twin). The id is then authorized twice, which is one duplicate audit event
// and one duplicate result row per repeat.
func TestMonitoringGuard_DuplicateSiteIDsCollapseToOne(t *testing.T) {
	id := uuid.New()
	rejected, authorized := partitionSiteIDs(orgPrincipal(uuid.New()),
		[]string{id.String(), id.String(), id.String(), "not-a-uuid", "not-a-uuid"})

	if len(authorized) != 1 {
		t.Fatalf("a repeated site id must be acted on once, got %d entries: %v", len(authorized), authorized)
	}
	if len(rejected) != 1 || rejected[0].Detail != "invalid_site_id" {
		t.Fatalf("a repeated unparseable id must be reported once, got %+v", rejected)
	}
}

// GUARD 3 — the 200-cap, at the HANDLER, before partitionSiteIDs walks the
// list.
//
// Mutation: delete the `len(ids) > maxBulkMonitoringSites` branch in
// checkMonitoringSiteIDs. 201 junk ids are then partitioned and answered 200
// with 201 result rows instead of a 422.
func TestMonitoringGuard_CapIsEnforcedBeforePartition(t *testing.T) {
	ids := make([]string, maxBulkMonitoringSites+1)
	for i := range ids {
		ids[i] = "not-a-uuid" // junk on purpose: the cheap valid-uuid path was never the broken one
	}
	rec := postMonitoring(t, "/api/v1/sites/monitoring/pause", map[string]any{"site_ids": ids})
	assertErrorCode(t, rec, http.StatusUnprocessableEntity, "too_many_sites")
}

// The service keeps its own copy of the cap for callers that are not the
// handler. Mutation: delete the service's cap branch; this reddens while the
// handler test above stays green, which is the point of having both.
//
// DELIBERATELY CALLS svc.PauseMonitoring DIRECTLY rather than through
// postMonitoring/HTTP: checkMonitoringSiteIDs at the handler (GUARD 3, above)
// always runs first and returns the same too_many_sites code, so an
// over-the-limit request never reaches the service in a state that could
// exercise its own cap. An HTTP-route test cannot observe this branch at all;
// calling the service directly is the only way to prove it independently
// enforces the limit rather than relying on the handler never being
// bypassed. It stops at the fake repo (never touches Postgres) because the
// thing under test is Go validation logic, not anything RLS or a DB role
// could affect.
func TestMonitoringGuard_CapIsAlsoEnforcedInTheService(t *testing.T) {
	svc, repo := newMonitoringSvc()
	tenant := uuid.New()
	ids := make([]uuid.UUID, maxBulkMonitoringSites+1)
	for i := range ids {
		ids[i] = uuid.New()
	}
	p := orgPrincipal(tenant)
	_, err := svc.PauseMonitoring(context.Background(), PauseMonitoringInput{
		TenantID: tenant, Principal: p, SiteIDs: ids,
	})
	requireDomainCode(t, err, "too_many_sites")
	if len(repo.pauseCalls) != 0 {
		t.Fatalf("an over-cap request must never reach the repo, got %d calls", len(repo.pauseCalls))
	}
}

// Exactly 200 must still be accepted — the guard must not over-fire.
func TestMonitoringGuard_CapAcceptsExactlyTheLimit(t *testing.T) {
	svc, repo := newMonitoringSvc()
	tenant := uuid.New()
	ids := make([]uuid.UUID, maxBulkMonitoringSites)
	for i := range ids {
		ids[i] = uuid.New()
	}
	if _, err := svc.PauseMonitoring(context.Background(), PauseMonitoringInput{
		TenantID: tenant, Principal: orgPrincipal(tenant), SiteIDs: ids,
	}); err != nil {
		t.Fatalf("exactly %d sites must be accepted, got %v", maxBulkMonitoringSites, err)
	}
	if len(repo.pauseCalls) != 1 {
		t.Fatalf("the at-limit request must reach the repo exactly once, got %d", len(repo.pauseCalls))
	}
}

// GUARD 4 — the reason-length cap.
//
// Mutation: delete the `len(in.Reason) > maxPauseReasonLen` branch. A pasted
// logfile is then stored in the column and copied into every audit row.
func TestMonitoringGuard_ReasonLengthCap(t *testing.T) {
	svc, repo := newMonitoringSvc()
	tenant := uuid.New()
	_, err := svc.PauseMonitoring(context.Background(), PauseMonitoringInput{
		TenantID: tenant, Principal: orgPrincipal(tenant),
		SiteIDs: []uuid.UUID{uuid.New()},
		Reason:  strings.Repeat("x", maxPauseReasonLen+1),
	})
	requireDomainCode(t, err, "reason_too_long")
	if len(repo.pauseCalls) != 0 {
		t.Fatalf("an over-long reason must never reach the repo, got %d calls", len(repo.pauseCalls))
	}

	// Exactly at the limit must pass: a cap that rejects legal input gets
	// switched off, and then it caps nothing.
	if _, err := svc.PauseMonitoring(context.Background(), PauseMonitoringInput{
		TenantID: tenant, Principal: orgPrincipal(tenant),
		SiteIDs: []uuid.UUID{uuid.New()},
		Reason:  strings.Repeat("x", maxPauseReasonLen),
	}); err != nil {
		t.Fatalf("a reason of exactly %d chars must be accepted, got %v", maxPauseReasonLen, err)
	}
}

// GUARD 5 — tenant_required.
//
// Mutation: delete the `in.TenantID == uuid.Nil` branch. The write then runs
// with a nil tenant: RunTenantTx sets app.tenant_id to the nil uuid and the
// explicit `tenant_id = $1` matches nothing, so it silently reports every site
// as not found instead of refusing an unauthenticated-shaped request.
func TestMonitoringGuard_TenantRequired(t *testing.T) {
	svc, repo := newMonitoringSvc()
	in := PauseMonitoringInput{
		TenantID: uuid.Nil, Principal: orgPrincipal(uuid.Nil), SiteIDs: []uuid.UUID{uuid.New()},
	}
	_, err := svc.PauseMonitoring(context.Background(), in)
	requireDomainCode(t, err, "tenant_required")

	_, err = svc.ResumeMonitoring(context.Background(), ResumeMonitoringInput{
		TenantID: uuid.Nil, Principal: orgPrincipal(uuid.Nil), SiteIDs: in.SiteIDs,
	})
	requireDomainCode(t, err, "tenant_required")

	if len(repo.pauseCalls)+len(repo.resumeCalls) != 0 {
		t.Fatalf("a tenantless request must never reach the repo, got %d pause + %d resume",
			len(repo.pauseCalls), len(repo.resumeCalls))
	}
}

// GUARD 6 — the principal_required branch that keeps the transaction from
// falling back to a tenant-wide scope.
//
// Mutation: delete it, or make monitoringTx fall back to InTenantTxAsUser on a
// nil principal. The nil-principal request then writes with the RESTRICTIVE
// site-scope policy inert, which is the exact defect this phase fixes.
func TestMonitoringGuard_PrincipalRequired(t *testing.T) {
	svc, repo := newMonitoringSvc()
	tenant := uuid.New()
	_, err := svc.PauseMonitoring(context.Background(), PauseMonitoringInput{
		TenantID: tenant, Principal: nil, SiteIDs: []uuid.UUID{uuid.New()},
	})
	requireDomainCode(t, err, "principal_required")
	if len(repo.pauseCalls) != 0 {
		t.Fatalf("a principal-less request must never reach the repo, got %d calls", len(repo.pauseCalls))
	}
}

// The principal REACHES the repo, which is what routes the write to
// InScopedTenantTx. Mutation: drop `Principal: p` from either handler; this
// fails on the nil, and the RESTRICTIVE policy would be inert again.
func TestMonitoringGuard_PrincipalReachesTheRepo(t *testing.T) {
	svc, repo := newMonitoringSvc()
	tenant, siteID := uuid.New(), uuid.New()
	p := scopedPrincipal(tenant, siteID)
	h := &Handler{svc: svc}

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	engine.POST("/pause", h.pauseMonitoring)
	engine.POST("/resume", h.resumeMonitoring)

	for _, path := range []string{"/pause", "/resume"} {
		body, _ := json.Marshal(map[string]any{"site_ids": []string{siteID.String()}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	if len(repo.pauseCalls) != 1 || repo.pauseCalls[0].Principal == nil {
		t.Fatalf("the pause must carry the principal to the repo, got %+v", repo.pauseCalls)
	}
	if repo.pauseCalls[0].Principal.GetScope() != domain.ScopeSite {
		t.Fatalf("the repo must see the SITE scope, got %q", repo.pauseCalls[0].Principal.GetScope())
	}
	if got := repo.pauseCalls[0].Principal.GetAllowedSiteIDs(); len(got) != 1 || got[0] != siteID {
		t.Fatalf("the repo must see the allowed site ids, got %v", got)
	}
	if len(repo.resumeCalls) != 1 || repo.resumeCalls[0].Principal == nil {
		t.Fatalf("the resume must carry the principal to the repo, got %+v", repo.resumeCalls)
	}
}

// GUARD 7 — invalid_resume_at, the zero-timestamp branch.
//
// Mutation: delete the `in.ResumeAt.IsZero()` branch. The zero instant then
// falls through to the in-the-past check and is reported as resume_at_in_past,
// telling the caller their timestamp is early when it is unparseable.
func TestMonitoringGuard_InvalidResumeAt(t *testing.T) {
	svc, repo := newMonitoringSvc()
	tenant := uuid.New()
	var zero time.Time
	_, err := svc.PauseMonitoring(context.Background(), PauseMonitoringInput{
		TenantID: tenant, Principal: orgPrincipal(tenant),
		SiteIDs: []uuid.UUID{uuid.New()}, ResumeAt: &zero,
	})
	requireDomainCode(t, err, "invalid_resume_at")
	if len(repo.pauseCalls) != 0 {
		t.Fatalf("a zero resume_at must never reach the repo, got %d calls", len(repo.pauseCalls))
	}
}

// GUARD 8 — nullableUUID, the API-key actor path.
//
// Mutation: make nullableUUID return &u unconditionally. monitoring_paused_by
// is then written as the all-zero uuid, which is not a users(id) row: the FK
// rejects it and every API-key pause 500s. Proved end to end against Postgres
// in tests/gh414_monitoring_guards_test.go; pinned here because this is the one
// line that decides it.
func TestMonitoringGuard_NullableUUIDMapsNilActorToNULL(t *testing.T) {
	if got := nullableUUID(uuid.Nil); got != nil {
		t.Fatalf("an API-key actor (uuid.Nil) must store NULL, got %v", *got)
	}
	id := uuid.New()
	got := nullableUUID(id)
	if got == nil || *got != id {
		t.Fatalf("a user actor must be stored as itself, got %v", got)
	}
}

// FIX 4 — the same mistake must not produce two different errors.
//
// A resume_at in the past is a 422 whether or not the authorization filter
// emptied the caller's list on the way in. Mutation: move the
// `len(in.SiteIDs) == 0` check back above the resume_at validation; the
// all-rejected caller then gets site_ids_required for a bad timestamp while
// the ordinary caller gets resume_at_in_past.
func TestMonitoringGuard_SameBadResumeAtForEveryCaller(t *testing.T) {
	svc, _ := newMonitoringSvc()
	tenant := uuid.New()
	past := time.Now().Add(-time.Hour)

	_, errWithSites := svc.PauseMonitoring(context.Background(), PauseMonitoringInput{
		TenantID: tenant, Principal: orgPrincipal(tenant),
		SiteIDs: []uuid.UUID{uuid.New()}, ResumeAt: &past,
	})
	_, errAllRejected := svc.PauseMonitoring(context.Background(), PauseMonitoringInput{
		TenantID: tenant, Principal: orgPrincipal(tenant),
		SiteIDs: nil, ResumeAt: &past,
	})
	requireDomainCode(t, errWithSites, "resume_at_in_past")
	requireDomainCode(t, errAllRejected, "resume_at_in_past")
}

// FIX 4 — an all-rejected list is the per-site report the route promises, not a
// 422 that hides it. Mutation: restore `site_ids_required` in the service for
// an empty list; the response becomes a 422 with no results array.
func TestMonitoringGuard_AllRejectedIDsStillGetTheReport(t *testing.T) {
	granted, ungranted := uuid.New(), uuid.New()
	svc, _ := newMonitoringSvc()
	h := &Handler{svc: svc}
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			domain.WithPrincipal(c.Request.Context(), scopedPrincipal(uuid.New(), granted)))
		c.Next()
	})
	engine.POST("/pause", h.pauseMonitoring)

	body, _ := json.Marshal(map[string]any{"site_ids": []string{ungranted.String(), "not-a-uuid"}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/pause", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("an all-rejected list must still be a 200 with the report, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Results []struct {
			SiteID string `json:"site_id"`
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		} `json:"results"`
		ChangedCount int `json:"changed_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if len(got.Results) != 2 || got.ChangedCount != 0 {
		t.Fatalf("expected 2 rejected results and changed_count 0, got %+v", got)
	}
	for _, r := range got.Results {
		if r.OK {
			t.Fatalf("a rejected id must not be ok:true: %+v", r)
		}
	}
}

// FIX 3 — an oversized body is refused BEFORE it is parsed.
//
// Mutation: delete the http.MaxBytesReader line in bindMonitoringBody. The
// 888 KB body is then read and unmarshalled in full; the request is still
// refused by the cap, but the work is now bounded by what the caller chose to
// send rather than by the cap, which is the whole distinction.
func TestMonitoringGuard_OversizedBodyIsRefusedBeforeParsing(t *testing.T) {
	junk := make([]string, 100_000)
	for i := range junk {
		junk[i] = "not-a-uuid-not-a-uuid-not-a-uuid"
	}
	body, err := json.Marshal(map[string]any{"site_ids": junk})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(body) <= maxMonitoringBodyBytes {
		t.Fatalf("the probe body must exceed the cap to test it, got %d bytes", len(body))
	}
	rec := postMonitoringRaw(t, "/api/v1/sites/monitoring/pause", body)
	assertErrorCode(t, rec, http.StatusUnprocessableEntity, "request_too_large")
	if rec.Body.Len() > 4096 {
		t.Fatalf("the refusal must be small, got a %d-byte response", rec.Body.Len())
	}
}

// A legal body at the largest documented size must NOT be refused — the honest
// case the size guard must not block.
func TestMonitoringGuard_LargestLegalBodyIsAccepted(t *testing.T) {
	ids := make([]string, maxBulkMonitoringSites)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	body, _ := json.Marshal(map[string]any{
		"site_ids": ids,
		"reason":   strings.Repeat("x", maxPauseReasonLen),
	})
	if len(body) >= maxMonitoringBodyBytes {
		t.Fatalf("the largest legal request (%d bytes) must fit under the %d-byte cap",
			len(body), maxMonitoringBodyBytes)
	}
	rec := postMonitoringRaw(t, "/api/v1/sites/monitoring/pause", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("the largest legal body must be accepted, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- helpers -------------------------------------------------------------

// postMonitoring mounts the real handler over the in-memory repo and posts a
// JSON body to it, with an org-scoped principal in context.
func postMonitoring(t *testing.T, path string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return postMonitoringRaw(t, path, body)
}

func postMonitoringRaw(t *testing.T, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc, _ := newMonitoringSvc()
	h := &Handler{svc: svc}
	tenant := uuid.New()

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), orgPrincipal(tenant)))
		c.Next()
	})
	v1 := engine.Group("/api/v1")
	v1.POST("/sites/monitoring/pause", h.pauseMonitoring)
	v1.POST("/sites/monitoring/resume", h.resumeMonitoring)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(rec, req)
	return rec
}

// assertErrorCode fails unless the response is `status` and its error envelope
// carries exactly `code`. Asserting the status alone would let any 422 pass,
// which is how "the same mistake, two different errors" survived review.
func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("expected status %d, got %d: %s", status, rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, rec.Body.String())
	}
	got := env.Error.Code
	if got == "" {
		got = env.Code
	}
	if got != code {
		t.Fatalf("expected error code %q, got %q (body %s)", code, got, rec.Body.String())
	}
}
