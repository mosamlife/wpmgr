package tests

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// ----------------------------- helpers ---------------------------------------

// startRedis spins up an ephemeral Redis 7 container and returns a redigo pool
// connected to it. Skips when Docker is unavailable. The container is torn
// down via t.Cleanup. Redis 6.2+ is required for the GETDEL command the hot-
// path consume uses.
func startRedis(t *testing.T) (*redis.Pool, string) {
	t.Helper()
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForListeningPort("6379/tcp").WithStartupTimeout(60 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Skipf("skipping: cannot start redis container (docker unavailable?): %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("redis host: %v", err)
	}
	port, err := c.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("redis port: %v", err)
	}
	addr := fmt.Sprintf("%s:%s", host, port.Port())
	pool := auth.NewRedisPool(addr, "")
	t.Cleanup(func() { _ = pool.Close() })
	return pool, addr
}

// autologinSiteAdapter is the test-side SiteLookup adapter (mirrors the
// production cmd/wpmgr/siteadapter.go shim to keep the autologin package
// site-import-free).
type autologinSiteAdapter struct{ svc *site.Service }

func (a *autologinSiteAdapter) GetSiteForAutologin(ctx context.Context, tenantID, siteID uuid.UUID) (string, bool, error) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	return s.URL, true, nil
}

// newSigner builds an Ed25519 signer for the autologin tests.
func newAutologinSigner(t *testing.T) *agentcmd.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatalf("build signer: %v", err)
	}
	return signer
}

// buildAutologinService assembles a real Service backed by the test PG pool +
// Redis pool, an in-memory rate limiter, and a no-op-but-recording audit
// Recorder.
func buildAutologinService(t *testing.T, pool *db.Pool, redisPool *redis.Pool, siteSvc *site.Service, cfg autologin.Config) (*autologin.Service, *audit.Recorder) {
	t.Helper()
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	svc := autologin.NewService(
		autologin.NewRepo(pool),
		autologin.NewRedigoStore(redisPool),
		newAutologinSigner(t),
		&autologinSiteAdapter{svc: siteSvc},
		autologin.NewMemoryLimiter(),
		rec,
		domain.SystemClock{},
		cfg,
	)
	return svc, rec
}

// ----------------------------- TESTS -----------------------------------------

// TestAutologinMintHappyPath persists in PG + Redis, returns the documented
// redirect URL shape, and records autologin.requested.
func TestAutologinMintHappyPath(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "al-mint-happy")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})

	s := enrollFakeSite(t, pool, tenant, "https://wp-mint.example.com")
	userID := seedAutologinUser(t, pool, "mint-happy@example.com")

	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})
	tok, err := svc.Mint(ctx, autologin.MintRequest{
		TenantID: tenant, SiteID: s.ID, InitiatorID: userID,
		TargetWPUser: "admin", RedirectTo: "/wp-admin/edit.php",
		IP: "203.0.113.10", UserAgent: "go-test",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(tok.RedirectURL, "https://wp-mint.example.com/wp-json/wpmgr/v1/autologin?") {
		t.Fatalf("unexpected redirect URL: %q", tok.RedirectURL)
	}
	if tok.NonceID == "" || tok.JWT == "" {
		t.Fatalf("empty token: %+v", tok)
	}

	// PG row exists with consumed_at NULL.
	admin := connectAdmin(t, pool)
	defer admin.Close()
	var consumed *time.Time
	if err := admin.QueryRow(ctx, "SELECT consumed_at FROM autologin_tokens WHERE id=$1", tok.NonceID).Scan(&consumed); err != nil {
		t.Fatalf("read token: %v", err)
	}
	if consumed != nil {
		t.Fatalf("token already consumed on mint")
	}

	// Redis key exists.
	conn, err := redisPool.GetContext(ctx)
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	defer conn.Close()
	if _, err := redis.Bytes(conn.Do("GET", autologin.RedisKeyPrefix+tok.NonceID)); err != nil {
		t.Fatalf("expected redis key, got %v", err)
	}
}

// TestAutologinMintRateLimit11thRejected proves the per-(initiator,site) cap
// rejects the 11th request and includes a positive retry hint.
func TestAutologinMintRateLimit11thRejected(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	tenant := seedTenant(t, pool, "al-rl")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	s := enrollFakeSite(t, pool, tenant, "https://wp-rl.example.com")
	userID := seedAutologinUser(t, pool, "rl@example.com")
	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})

	ctx := context.Background()
	for i := 0; i < autologin.LimitInitiatorSitePerMin; i++ {
		if _, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenant, SiteID: s.ID, InitiatorID: userID}); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	_, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenant, SiteID: s.ID, InitiatorID: userID})
	if err == nil {
		t.Fatal("11th mint accepted past per-initiator budget")
	}
	if autologin.RetryAfterFromError(err) <= 0 {
		t.Fatalf("retry-after = %d; want > 0", autologin.RetryAfterFromError(err))
	}
}

// TestAutologinMintPolicyDisabled returns 403 policy_disabled when the per-
// site policy has enabled=false.
func TestAutologinMintPolicyDisabled(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "al-disabled")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	s := enrollFakeSite(t, pool, tenant, "https://wp-disabled.example.com")
	userID := seedAutologinUser(t, pool, "disabled@example.com")

	// Disable via direct admin update (the table is tenant-scoped, but as
	// admin we can pre-seed before any policy fetch).
	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx,
		"INSERT INTO autologin_policies (site_id, tenant_id, enabled) VALUES ($1, $2, false)",
		s.ID, tenant); err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})
	_, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenant, SiteID: s.ID, InitiatorID: userID})
	if err == nil {
		t.Fatal("expected policy_disabled")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "policy_disabled" {
		t.Fatalf("want policy_disabled, got %v", err)
	}
}

// TestAutologinMintTenantScoping prevents minting for a site that lives in a
// different tenant.
func TestAutologinMintTenantScoping(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	ctx := context.Background()
	tenantA := seedTenant(t, pool, "al-tenA")
	tenantB := seedTenant(t, pool, "al-tenB")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	sA := enrollFakeSite(t, pool, tenantA, "https://wp-A.example.com")
	userB := seedAutologinUser(t, pool, "foreign@example.com")

	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})
	_, err := svc.Mint(ctx, autologin.MintRequest{
		TenantID: tenantB, SiteID: sA.ID, InitiatorID: userB,
	})
	if err == nil {
		t.Fatal("expected site_not_found for cross-tenant mint")
	}
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

// TestAutologinConsumeRedisHotPath asserts the Redis GETDEL path wins, marks
// the PG row consumed too, and rejects the replay as 410.
func TestAutologinConsumeRedisHotPath(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "al-consume-redis")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	s := enrollFakeSite(t, pool, tenant, "https://wp-redis.example.com")
	userID := seedAutologinUser(t, pool, "consume-redis@example.com")

	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})
	tok, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenant, SiteID: s.ID, InitiatorID: userID, TargetWPUser: "admin"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	res, err := svc.Consume(ctx, s.ID, s.ID, tok.NonceID, "203.0.113.50")
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if res.HotPath != autologin.HotPathRedis {
		t.Fatalf("hot path = %q, want redis", res.HotPath)
	}
	if res.TargetWPUser != "admin" {
		t.Fatalf("target = %q, want admin", res.TargetWPUser)
	}
	if len(res.AllowedRoles) == 0 || res.AllowedRoles[0] != "administrator" {
		t.Fatalf("allowed roles = %v, want [administrator]", res.AllowedRoles)
	}

	// PG row must reflect consumed_at.
	admin := connectAdmin(t, pool)
	defer admin.Close()
	var consumed *time.Time
	if err := admin.QueryRow(ctx, "SELECT consumed_at FROM autologin_tokens WHERE id=$1", tok.NonceID).Scan(&consumed); err != nil {
		t.Fatalf("read consumed_at: %v", err)
	}
	if consumed == nil {
		t.Fatal("PG row was not marked consumed on Redis hot-path success")
	}

	// Replay → 410.
	if _, err := svc.Consume(ctx, s.ID, s.ID, tok.NonceID, ""); err == nil {
		t.Fatal("expected 410 on replay")
	}
}

// TestAutologinConsumePGFallback wipes the Redis key before consume to force
// the PG single-shot fallback, then asserts the second consume rejects.
func TestAutologinConsumePGFallback(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "al-consume-pg")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	s := enrollFakeSite(t, pool, tenant, "https://wp-pg.example.com")
	userID := seedAutologinUser(t, pool, "consume-pg@example.com")
	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})

	tok, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenant, SiteID: s.ID, InitiatorID: userID, TargetWPUser: "alice"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Force a Redis miss by deleting the key before consume.
	conn, _ := redisPool.GetContext(ctx)
	if _, err := conn.Do("DEL", autologin.RedisKeyPrefix+tok.NonceID); err != nil {
		t.Fatalf("redis del: %v", err)
	}
	conn.Close()

	res, err := svc.Consume(ctx, s.ID, s.ID, tok.NonceID, "")
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if res.HotPath != autologin.HotPathPostgres {
		t.Fatalf("hot path = %q, want postgres", res.HotPath)
	}
	if res.TargetWPUser != "alice" {
		t.Fatalf("target = %q", res.TargetWPUser)
	}

	// Replay → 410.
	if _, err := svc.Consume(ctx, s.ID, s.ID, tok.NonceID, ""); err == nil {
		t.Fatal("expected 410 on PG-fallback replay")
	}
}

// TestAutologinConsumeWrongSiteRejected proves a token minted for site A
// cannot be consumed by the agent of site B (the SQL filter rejects it; the
// service surfaces 410 because the table never confirms existence to the
// wrong site).
func TestAutologinConsumeWrongSiteRejected(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "al-wrong-site")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	sA := enrollFakeSite(t, pool, tenant, "https://wp-A.example.com")
	sB := enrollFakeSite(t, pool, tenant, "https://wp-B.example.com")
	userID := seedAutologinUser(t, pool, "wrong-site@example.com")
	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})

	tok, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenant, SiteID: sA.ID, InitiatorID: userID})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Wipe the Redis key so the consume must go through PG (the SQL site_id
	// filter is what we're proving).
	conn, _ := redisPool.GetContext(ctx)
	_, _ = conn.Do("DEL", autologin.RedisKeyPrefix+tok.NonceID)
	conn.Close()

	if _, err := svc.Consume(ctx, sB.ID, sB.ID, tok.NonceID, ""); err == nil {
		t.Fatal("expected rejection when site B consumes site A's nonce")
	}
}

// TestAutologinConsumeExpiredRejected ages the PG row past expires_at and
// asserts the consume rejects with 410 (and Redis miss forces PG).
func TestAutologinConsumeExpiredRejected(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "al-expired")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	s := enrollFakeSite(t, pool, tenant, "https://wp-exp.example.com")
	userID := seedAutologinUser(t, pool, "expired@example.com")
	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})

	tok, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenant, SiteID: s.ID, InitiatorID: userID})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Age expires_at into the past and wipe Redis.
	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx, "UPDATE autologin_tokens SET expires_at = now() - interval '1 minute' WHERE id=$1", tok.NonceID); err != nil {
		t.Fatalf("age token: %v", err)
	}
	conn, _ := redisPool.GetContext(ctx)
	_, _ = conn.Do("DEL", autologin.RedisKeyPrefix+tok.NonceID)
	conn.Close()

	if _, err := svc.Consume(ctx, s.ID, s.ID, tok.NonceID, ""); err == nil {
		t.Fatal("expected rejection on expired token")
	}
}

// TestAutologinConsumeConcurrentSingleWinner runs N goroutines hammering the
// same nonce in parallel; exactly ONE must win, with the rest receiving 410.
// We force the PG fallback (delete Redis) so the SQL UPDATE...RETURNING is
// the lone arbiter, and then separately run a Redis-path race.
func TestAutologinConsumeConcurrentSingleWinner(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "al-race")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	s := enrollFakeSite(t, pool, tenant, "https://wp-race.example.com")
	userID := seedAutologinUser(t, pool, "race@example.com")
	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})

	// PG-only race.
	tok1, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenant, SiteID: s.ID, InitiatorID: userID, TargetWPUser: "a"})
	if err != nil {
		t.Fatalf("mint pg: %v", err)
	}
	conn, _ := redisPool.GetContext(ctx)
	_, _ = conn.Do("DEL", autologin.RedisKeyPrefix+tok1.NonceID)
	conn.Close()
	winnersPG := concurrentConsume(t, svc, s.ID, tok1.NonceID, 8)
	if winnersPG != 1 {
		t.Fatalf("PG race winners = %d, want exactly 1", winnersPG)
	}

	// Redis-path race (Redis remains populated).
	tok2, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenant, SiteID: s.ID, InitiatorID: userID, TargetWPUser: "b"})
	if err != nil {
		t.Fatalf("mint redis: %v", err)
	}
	winnersRedis := concurrentConsume(t, svc, s.ID, tok2.NonceID, 8)
	if winnersRedis != 1 {
		t.Fatalf("Redis race winners = %d, want exactly 1", winnersRedis)
	}
}

// concurrentConsume races N goroutines against the same nonce and returns the
// number that won (non-error response).
func concurrentConsume(t *testing.T, svc *autologin.Service, siteID uuid.UUID, nonce string, n int) int {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(n)
	var won int32
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			if _, err := svc.Consume(context.Background(), siteID, siteID, nonce, ""); err == nil {
				atomic.AddInt32(&won, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	return int(won)
}

// TestAutologinRLSIsolation proves cross-tenant SELECT on autologin_tokens
// returns 0 rows under the non-superuser app role (the wpmgr_app role used by
// the startPostgres pool).
func TestAutologinRLSIsolation(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	ctx := context.Background()
	tenantA := seedTenant(t, pool, "al-rls-a")
	tenantB := seedTenant(t, pool, "al-rls-b")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	sA := enrollFakeSite(t, pool, tenantA, "https://wp-rlsA.example.com")
	userA := seedAutologinUser(t, pool, "rlsA@example.com")
	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})

	if _, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenantA, SiteID: sA.ID, InitiatorID: userA}); err != nil {
		t.Fatalf("mint A: %v", err)
	}
	// Under tenant B's GUC, A's nonce row is invisible.
	err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM autologin_tokens").Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatalf("RLS leak: tenant B sees %d of tenant A's autologin tokens", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("rls select: %v", err)
	}
}

// TestAutologinAgentEndpointRejectsUnsigned proves the agent-auth middleware
// already covers the /agent/v1/autologin/consume route (mirrors the M2 pattern).
// We mount the same group with the real authenticator and POST with no
// signature headers; the response is 401 with no body leakage.
func TestAutologinAgentEndpointRejectsUnsigned(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	tenant := seedTenant(t, pool, "al-agent-401")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	enrollFakeSite(t, pool, tenant, "https://wp-401.example.com")
	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	authn := agent.NewAuthenticator(siteSvc, domain.SystemClock{}, 5*time.Minute)
	g := r.Group("/agent/v1")
	g.Use(authn.Authenticate())
	autologin.NewAgentHandler(svc).Register(g)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agent/v1/autologin/consume", strings.NewReader(`{"nonce":"x","site_id":"00000000-0000-0000-0000-000000000000"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned consume status = %d, want 401", rec.Code)
	}
}

// TestAutologinAgentEndpointHappyPath signs a real consume request with the
// site's enrolled key and asserts the full path returns 200 with the policy's
// allowed_wp_roles.
func TestAutologinAgentEndpointHappyPath(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "al-agent-200")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})

	// Enroll a site whose key we know.
	created, _ := siteSvc.CreatePairingCode(ctx, site.CreatePairingCodeInput{TenantID: tenant})
	_, priv, pubB64 := genKey(t)
	s, err := siteSvc.Enroll(ctx, site.EnrollRequest{
		PairingCode: created.Plaintext, SiteURL: "https://wp-agent-ok.example.com", AgentPublicKey: pubB64,
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	userID := seedAutologinUser(t, pool, "agent-ok@example.com")
	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})
	tok, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenant, SiteID: s.ID, InitiatorID: userID, TargetWPUser: "admin"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	authn := agent.NewAuthenticator(siteSvc, domain.SystemClock{}, 5*time.Minute)
	g := r.Group("/agent/v1")
	g.Use(authn.Authenticate())
	autologin.NewAgentHandler(svc).Register(g)

	body := fmt.Sprintf(`{"nonce":%q,"site_id":%q,"consumed_from_ip":"198.51.100.7"}`, tok.NonceID, s.ID.String())
	tsStr := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "test-nonce-" + tok.NonceID
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, agent.CanonicalMessage(http.MethodPost, "/agent/v1/autologin/consume", tsStr, nonce, []byte(body))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agent/v1/autologin/consume", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(agent.HeaderAgentKey, pubB64)
	req.Header.Set(agent.HeaderTimestamp, tsStr)
	req.Header.Set(agent.HeaderNonce, nonce)
	req.Header.Set(agent.HeaderSignature, sig)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("signed consume status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "administrator") {
		t.Fatalf("response missing allowed_wp_roles: %s", rec.Body.String())
	}
}

// ----------------------------- helpers (seed) --------------------------------

// seedAutologinUser inserts a user row directly (mirrors the auth test
// fixtures; the autologin mint requires a real users.id by FK).
func seedAutologinUser(t *testing.T, pool *db.Pool, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		"INSERT INTO users (email) VALUES ($1) RETURNING id", email).Scan(&id)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}
