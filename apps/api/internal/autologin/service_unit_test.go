package autologin

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// fakeSiteLookup answers GetSiteForAutologin from a static map keyed by
// (tenant, site). Missing entries return ok=false (the service maps to 404).
type fakeSiteLookup struct {
	urls map[uuid.UUID]string // keyed by site id
}

func (f *fakeSiteLookup) GetSiteForAutologin(_ context.Context, _, siteID uuid.UUID) (string, bool, error) {
	url, ok := f.urls[siteID]
	return url, ok, nil
}

// fakeSigner returns a deterministic token + jti so tests can assert on
// claim/aud routing without exercising ed25519.
type fakeSigner struct {
	mu   sync.Mutex
	seen []struct{ Aud, Tgt string }
	jti  int
}

func (f *fakeSigner) MintAutologin(_ time.Time, aud, tgt string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, struct{ Aud, Tgt string }{aud, tgt})
	f.jti++
	jti := "nonce-" + itoaTest(f.jti)
	return "jwt:" + aud + ":" + tgt + ":" + jti, jti, nil
}

// fakeRepo is an in-memory Repo with no RLS / SQL. Sufficient for unit tests
// of the service's branching; the testcontainers tests cover the real path.
type fakeRepo struct {
	mu       sync.Mutex
	tokens   map[string]InsertTokenInput // by nonce id
	consumed map[string]bool
	policies map[uuid.UUID]Policy // by site
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		tokens:   map[string]InsertTokenInput{},
		consumed: map[string]bool{},
		policies: map[uuid.UUID]Policy{},
	}
}

func (r *fakeRepo) InsertToken(_ context.Context, in InsertTokenInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens[in.NonceID] = in
	return nil
}

func (r *fakeRepo) GetOrCreatePolicy(_ context.Context, tenantID, siteID uuid.UUID) (Policy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.policies[siteID]; ok {
		return p, nil
	}
	p := Policy{SiteID: siteID, TenantID: tenantID, Enabled: true, AllowedWPRoles: DefaultAllowedWPRoles, MaxSessionAgeMinutes: 30}
	r.policies[siteID] = p
	return p, nil
}

func (r *fakeRepo) ConsumeToken(_ context.Context, nonceID string, siteID uuid.UUID, _ string) (ConsumedRow, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tokens[nonceID]
	if !ok || r.consumed[nonceID] || t.SiteID != siteID {
		return ConsumedRow{}, false, nil
	}
	r.consumed[nonceID] = true
	return ConsumedRow{
		NonceID:           t.NonceID,
		TenantID:          t.TenantID,
		SiteID:            t.SiteID,
		InitiatorUserID:   t.InitiatorUserID,
		TargetWPUserLogin: t.TargetWPUserLogin,
	}, true, nil
}

func (r *fakeRepo) GetPolicyForAgent(_ context.Context, siteID uuid.UUID) (Policy, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.policies[siteID]
	return p, ok, nil
}

// fakeStore mimics Redis with no TTL handling — enough to assert the
// Redis-hit and Redis-miss code paths in service.Consume.
type fakeStore struct {
	mu      sync.Mutex
	values  map[string]RedisPayload
	failGet bool
}

func newFakeStore() *fakeStore { return &fakeStore{values: map[string]RedisPayload{}} }

func (s *fakeStore) Set(_ context.Context, k string, p RedisPayload, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[k] = p
	return nil
}

func (s *fakeStore) ConsumeOnce(_ context.Context, k string) (RedisPayload, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failGet {
		return RedisPayload{}, false, context.DeadlineExceeded
	}
	p, ok := s.values[k]
	if !ok {
		return RedisPayload{}, false, nil
	}
	delete(s.values, k)
	return p, true, nil
}

// nopRecorder satisfies Recorder without writing audit rows.
type nopRecorder struct{ entries []audit.Event }

func (n *nopRecorder) Record(_ context.Context, e audit.Event) (audit.Entry, error) {
	n.entries = append(n.entries, e)
	return audit.Entry{ID: uuid.New()}, nil
}

// itoaTest is a local strconv-less itoa for the fake signer.
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func newSvc(t *testing.T, repo Repo, store NonceStore, signer Signer, sites SiteLookup) *Service {
	t.Helper()
	return NewService(repo, store, signer, sites, NewMemoryLimiter(), &nopRecorder{}, domain.SystemClock{}, Config{})
}

// TestServiceMintHappyPath proves a green mint returns a redirect URL of the
// documented shape, persists the nonce in BOTH stores (PG + Redis), and
// records autologin.requested without leaking the JWT.
func TestServiceMintHappyPath(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	userID := uuid.New()
	repo := newFakeRepo()
	store := newFakeStore()
	signer := &fakeSigner{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	rec := &nopRecorder{}

	svc := NewService(repo, store, signer, sites, NewMemoryLimiter(), rec, domain.SystemClock{}, Config{})

	tok, err := svc.Mint(context.Background(), MintRequest{
		TenantID: tenantID, SiteID: siteID, InitiatorID: userID,
		TargetWPUser: "admin", RedirectTo: "/wp-admin/edit.php",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok.NonceID == "" || tok.JWT == "" {
		t.Fatalf("empty token: %+v", tok)
	}
	if !strings.HasPrefix(tok.RedirectURL, "https://wp.example.com/wp-json/wpmgr/v1/autologin?") {
		t.Fatalf("redirect_url shape wrong: %q", tok.RedirectURL)
	}
	// url.Values.Encode() escapes ':' as %3A; re-parse to compare structurally.
	if !strings.Contains(tok.RedirectURL, "token=") {
		t.Fatalf("redirect_url missing token=: %q", tok.RedirectURL)
	}
	if !strings.Contains(tok.RedirectURL, "redirect_to=") {
		t.Fatalf("redirect_url missing redirect_to: %q", tok.RedirectURL)
	}

	// PG + Redis both populated.
	if _, ok := repo.tokens[tok.NonceID]; !ok {
		t.Fatal("nonce not persisted in PG")
	}
	if _, ok := store.values[tok.NonceID]; !ok {
		t.Fatal("nonce not persisted in Redis")
	}

	// Audit captured the requested event — and never the JWT.
	if len(rec.entries) != 1 || rec.entries[0].Action != audit.ActionAutologinRequested {
		t.Fatalf("expected single autologin.requested audit, got %+v", rec.entries)
	}
	for _, v := range rec.entries[0].Metadata {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "jwt:") {
			t.Fatal("audit metadata leaked the JWT")
		}
	}
}

// TestServiceMintRateLimits exhausts the per-initiator bucket and asserts the
// next call is 429 with a positive retry hint.
func TestServiceMintRateLimits(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	userID := uuid.New()
	repo := newFakeRepo()
	store := newFakeStore()
	signer := &fakeSigner{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	svc := newSvc(t, repo, store, signer, sites)
	ctx := context.Background()

	for i := 0; i < LimitInitiatorSitePerMin; i++ {
		if _, err := svc.Mint(ctx, MintRequest{TenantID: tenantID, SiteID: siteID, InitiatorID: userID}); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	_, err := svc.Mint(ctx, MintRequest{TenantID: tenantID, SiteID: siteID, InitiatorID: userID})
	if err == nil {
		t.Fatal("expected rate-limit rejection after exhausting per-initiator budget")
	}
	if RetryAfterFromError(err) <= 0 {
		t.Fatalf("retry_after_seconds = %d; want > 0", RetryAfterFromError(err))
	}
}

// TestServiceMintSiteCap mirrors the per-site cap with two distinct initiators.
func TestServiceMintSiteCap(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	repo := newFakeRepo()
	store := newFakeStore()
	signer := &fakeSigner{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	svc := newSvc(t, repo, store, signer, sites)
	ctx := context.Background()

	// Distribute across many initiators so the per-initiator cap never fires.
	for i := 0; i < LimitSitePerMin; i++ {
		u := uuid.New()
		if _, err := svc.Mint(ctx, MintRequest{TenantID: tenantID, SiteID: siteID, InitiatorID: u}); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	u := uuid.New()
	_, err := svc.Mint(ctx, MintRequest{TenantID: tenantID, SiteID: siteID, InitiatorID: u})
	if err == nil {
		t.Fatal("expected per-site rate-limit rejection")
	}
}

// TestServiceMintPolicyDisabled returns 403 policy_disabled.
func TestServiceMintPolicyDisabled(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	userID := uuid.New()
	repo := newFakeRepo()
	repo.policies[siteID] = Policy{SiteID: siteID, TenantID: tenantID, Enabled: false}
	store := newFakeStore()
	signer := &fakeSigner{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	svc := newSvc(t, repo, store, signer, sites)

	_, err := svc.Mint(context.Background(), MintRequest{TenantID: tenantID, SiteID: siteID, InitiatorID: userID})
	if err == nil {
		t.Fatal("expected policy_disabled error")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "policy_disabled" {
		t.Fatalf("want policy_disabled, got %v", err)
	}
}

// TestServiceMint2FAFeatureFlagOffNeverTriggers proves that even with the
// per-site policy set, the 2FA gate is skipped when the global feature flag
// (WPMGR_AUTOLOGIN_REQUIRE_2FA_STEP_UP) is false.
func TestServiceMint2FAFeatureFlagOffNeverTriggers(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	userID := uuid.New()
	repo := newFakeRepo()
	repo.policies[siteID] = Policy{
		SiteID: siteID, TenantID: tenantID, Enabled: true,
		Require2FAStepUp: true, MaxSessionAgeMinutes: 30,
	}
	store := newFakeStore()
	signer := &fakeSigner{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	// Global flag default-off — even an ancient session must pass.
	svc := NewService(repo, store, signer, sites, NewMemoryLimiter(), &nopRecorder{}, domain.SystemClock{}, Config{Require2FAStepUp: false})

	_, err := svc.Mint(context.Background(), MintRequest{
		TenantID: tenantID, SiteID: siteID, InitiatorID: userID,
		SessionAge: 6 * time.Hour,
	})
	if err != nil {
		t.Fatalf("global 2FA flag off should ignore per-site policy: %v", err)
	}
}

// TestServiceMintSiteNotInTenant 404s when SiteLookup returns ok=false.
func TestServiceMintSiteNotInTenant(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	userID := uuid.New()
	repo := newFakeRepo()
	store := newFakeStore()
	signer := &fakeSigner{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{}} // empty — site does not exist for this tenant
	svc := newSvc(t, repo, store, signer, sites)

	_, err := svc.Mint(context.Background(), MintRequest{TenantID: tenantID, SiteID: siteID, InitiatorID: userID})
	if err == nil {
		t.Fatal("expected site_not_found")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

// TestServiceConsumeRedisHotPath proves a Redis hit also drives the PG
// atomic ConsumeToken (PG is the single arbiter — Redis is the cache that
// avoids a round-trip when the cluster has Redis available).
func TestServiceConsumeRedisHotPath(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	userID := uuid.New()
	repo := newFakeRepo()
	store := newFakeStore()
	signer := &fakeSigner{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	svc := newSvc(t, repo, store, signer, sites)

	tok, err := svc.Mint(context.Background(), MintRequest{
		TenantID: tenantID, SiteID: siteID, InitiatorID: userID, TargetWPUser: "alice",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	res, err := svc.Consume(context.Background(), siteID, siteID, tok.NonceID, "203.0.113.10")
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if res.HotPath != HotPathRedis {
		t.Fatalf("hot path = %q, want %q", res.HotPath, HotPathRedis)
	}
	if res.TargetWPUser != "alice" {
		t.Fatalf("target = %q, want alice", res.TargetWPUser)
	}
	// PG ConsumeToken also ran (the single arbiter) and the fakeRepo marked
	// the nonce consumed too.
	if !repo.consumed[tok.NonceID] {
		t.Fatal("PG ConsumeToken was not driven on the Redis hot path")
	}

	// Replay (Redis already deleted, PG marked consumed) → 410.
	if _, err := svc.Consume(context.Background(), siteID, siteID, tok.NonceID, "1.2.3.4"); err == nil {
		t.Fatal("expected 410 on replay after Redis-path consume")
	}
}

// TestServiceConsumePGFallback proves the PG fallback path wins when Redis
// misses (transport error or already-deleted-but-PG-still-pending).
func TestServiceConsumePGFallback(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	userID := uuid.New()
	repo := newFakeRepo()
	store := newFakeStore()
	signer := &fakeSigner{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	svc := newSvc(t, repo, store, signer, sites)

	tok, err := svc.Mint(context.Background(), MintRequest{
		TenantID: tenantID, SiteID: siteID, InitiatorID: userID, TargetWPUser: "bob",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Simulate a Redis transport failure so the fallback fires.
	store.failGet = true

	res, err := svc.Consume(context.Background(), siteID, siteID, tok.NonceID, "")
	if err != nil {
		t.Fatalf("consume (PG fallback): %v", err)
	}
	if res.HotPath != HotPathPostgres {
		t.Fatalf("hot path = %q, want %q", res.HotPath, HotPathPostgres)
	}

	// Second consume must be 410 (PG row already consumed).
	if _, err := svc.Consume(context.Background(), siteID, siteID, tok.NonceID, ""); err == nil {
		t.Fatal("expected 410 on second consume")
	}
}

// TestServiceConsumeSiteMismatch returns 403 site_mismatch when the claimed
// body site_id disagrees with the verified agent identity.
func TestServiceConsumeSiteMismatch(t *testing.T) {
	siteID := uuid.New()
	otherSiteID := uuid.New()
	repo := newFakeRepo()
	store := newFakeStore()
	signer := &fakeSigner{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	svc := newSvc(t, repo, store, signer, sites)

	_, err := svc.Consume(context.Background(), siteID, otherSiteID, "nonce-1", "")
	if err == nil {
		t.Fatal("expected site_mismatch")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "site_mismatch" {
		t.Fatalf("want site_mismatch, got %v", err)
	}
}

// TestBuildRedirectURL exercises the documented redirect_url shape.
func TestBuildRedirectURL(t *testing.T) {
	got, err := buildRedirectURL("https://wp.example.com/", "/wp-json/wpmgr/v1/autologin", "ey.JWT.X", "/wp-admin/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "https://wp.example.com/wp-json/wpmgr/v1/autologin?") {
		t.Fatalf("unexpected prefix: %s", got)
	}
	if !strings.Contains(got, "token=ey.JWT.X") {
		t.Fatalf("token missing: %s", got)
	}
	if !strings.Contains(got, "redirect_to=") {
		t.Fatalf("redirect_to missing: %s", got)
	}

	// Reject non-http(s).
	if _, err := buildRedirectURL("ftp://nope", "/x", "j", ""); err == nil {
		t.Fatal("ftp:// should be rejected")
	}
}

// TestRetryAfterFromError parses the canonical message.
func TestRetryAfterFromError(t *testing.T) {
	err := rateLimited(45 * time.Second)
	if sec := RetryAfterFromError(err); sec != 45 {
		t.Fatalf("retry-after = %d, want 45", sec)
	}
	if sec := RetryAfterFromError(domain.NotFound("x", "y")); sec != 0 {
		t.Fatalf("non-rate-limited error returned retry-after = %d", sec)
	}
}
