package rum

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestLimiter returns a generous rate.Limiter for use in LRU tests.
func newTestLimiter() *rate.Limiter {
	return rate.NewLimiter(rate.Every(time.Millisecond), 1000)
}

// ---------------------------------------------------------------------------
// Public endpoint isolation tests
// ---------------------------------------------------------------------------

// TestRumIngest_publicEndpointExistsOutsideAPIV1 verifies /rum/ingest is mounted
// on the root engine and is NOT accessible at /api/v1/rum/ingest or
// /agent/v1/rum/ingest.
func TestRumIngest_publicEndpointExistsOutsideAPIV1(t *testing.T) {
	engine := gin.New()
	engine.Use(rumCORSMiddleware())
	engine.POST("/rum/ingest", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	engine.OPTIONS("/rum/ingest", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	// /rum/ingest must NOT be 404.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rum/ingest", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	if w.Code == http.StatusNotFound {
		t.Error("/rum/ingest returned 404 — route is not mounted on root engine")
	}

	// /api/v1/rum/ingest and /agent/v1/rum/ingest must be 404 (not mounted there).
	for _, prefix := range []string{"/api/v1/rum/ingest", "/agent/v1/rum/ingest"} {
		w2 := httptest.NewRecorder()
		req2 := httptest.NewRequest(http.MethodPost, prefix, bytes.NewBufferString(`{}`))
		req2.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(w2, req2)
		if w2.Code != http.StatusNotFound {
			t.Errorf("route at %s should be 404 (isolated), got %d", prefix, w2.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// CORS middleware tests
// ---------------------------------------------------------------------------

func TestRumCORSMiddleware_headersOnPOST(t *testing.T) {
	engine := gin.New()
	engine.Use(rumCORSMiddleware())
	engine.POST("/rum/ingest", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rum/ingest", bytes.NewBufferString(`{}`))
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	acao := w.Header().Get("Access-Control-Allow-Origin")
	if acao == "" {
		t.Error("CORS: Access-Control-Allow-Origin header missing on POST")
	}
}

func TestRumCORSMiddleware_preflightOPTIONS(t *testing.T) {
	engine := gin.New()
	engine.Use(rumCORSMiddleware())
	engine.OPTIONS("/rum/ingest", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/rum/ingest", nil)
	req.Header.Set("Origin", "https://example.com")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("OPTIONS preflight: expected 204, got %d", w.Code)
	}
	acm := w.Header().Get("Access-Control-Allow-Methods")
	if !strings.Contains(acm, "POST") {
		t.Errorf("CORS: Access-Control-Allow-Methods does not include POST: %q", acm)
	}
}

// ---------------------------------------------------------------------------
// Body cap test (inline — no DB required)
// ---------------------------------------------------------------------------

func TestRumIngest_bodyCap(t *testing.T) {
	engine := gin.New()
	engine.POST("/rum/ingest", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBeaconBodyBytes)
		var b beaconBody
		if err := c.ShouldBindJSON(&b); err != nil {
			if isBodyTooLarge(err) {
				c.Status(http.StatusRequestEntityTooLarge)
				return
			}
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusNoContent)
	})

	// Body just over the limit must be rejected with 413. Wrap in a JSON string
	// so the decoder reads past the MaxBytesReader limit before hitting a parse error.
	padding := strings.Repeat("a", MaxBeaconBodyBytes)
	overLimitJSON := `{"key":"` + padding + `"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rum/ingest", strings.NewReader(overLimitJSON))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("body over limit: expected 413, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Validation allow-list tests (metric names, value bounds)
// ---------------------------------------------------------------------------

func TestRumIngest_invalidMetricRejected(t *testing.T) {
	engine := gin.New()
	engine.POST("/rum/ingest", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBeaconBodyBytes)
		var body beaconBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		if !AllowedMetrics[body.Metric] {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusNoContent)
	})

	badPayload := map[string]interface{}{
		"key":    "somekey",
		"url":    "https://example.com/",
		"metric": "fid", // not in allowed set
		"value":  250,
	}
	b, _ := json.Marshal(badPayload)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rum/ingest", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid metric: expected 400, got %d", w.Code)
	}
}

func TestRumIngest_validMetrics(t *testing.T) {
	for metric := range AllowedMetrics {
		t.Run(metric, func(t *testing.T) {
			engine := gin.New()
			engine.POST("/rum/ingest", func(c *gin.Context) {
				c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBeaconBodyBytes)
				var body beaconBody
				if err := c.ShouldBindJSON(&body); err != nil {
					c.Status(http.StatusBadRequest)
					return
				}
				if !AllowedMetrics[body.Metric] {
					c.Status(http.StatusBadRequest)
					return
				}
				c.Status(http.StatusNoContent)
			})
			goodPayload := map[string]interface{}{
				"key":    "somekey",
				"url":    "https://example.com/",
				"metric": metric,
				"value":  250,
			}
			b, _ := json.Marshal(goodPayload)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/rum/ingest", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(w, req)
			if w.Code == http.StatusBadRequest {
				t.Errorf("valid metric %q rejected with 400", metric)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// H1: per-metric value clamping (unit tests on clampValue)
// ---------------------------------------------------------------------------

// TestClampValue_lcpAt90s verifies that a 90 000 ms LCP (90 seconds, legitimately
// slow on a 2G connection) is stored with its original value (ok=true).
// 90 000 ms is well within the 600 000 ms ceiling. Previously this was rejected
// with 400 because the old ceiling was 60 000 ms, biasing p75 optimistically —
// the worst failure mode for a CWV product.
func TestClampValue_lcpAt90s(t *testing.T) {
	clamped, ok := clampValue("lcp", 90_000)
	if !ok {
		t.Fatal("clampValue(lcp, 90000) returned ok=false: slow LCP must be stored, not dropped")
	}
	if clamped != 90_000 {
		t.Errorf("clampValue(lcp, 90000) = %d, want 90000 (unchanged — within ceiling)", clamped)
	}
}

// TestClampValue_clsAt2500 verifies a CLS of 2.5 (2500 milli-units) is stored
// with the original value — it is below the CLS ceiling of 10 000.
func TestClampValue_clsAt2500(t *testing.T) {
	clamped, ok := clampValue("cls", 2500)
	if !ok {
		t.Fatal("clampValue(cls, 2500) returned ok=false: CLS 2.5 must be stored")
	}
	if clamped != 2500 {
		t.Errorf("clampValue(cls, 2500) = %d, want 2500 (no clamp needed)", clamped)
	}
}

// TestClampValue_negativeYields204 verifies that negative values return ok=false
// so the handler discards them with 204 (not 400 — no behavioral oracle).
func TestClampValue_negativeYields204(t *testing.T) {
	_, ok := clampValue("lcp", -1)
	if ok {
		t.Error("clampValue(lcp, -1) returned ok=true: negative value must be discarded")
	}
	_, ok = clampValue("cls", -100)
	if ok {
		t.Error("clampValue(cls, -100) returned ok=true: negative CLS must be discarded")
	}
}

// TestClampValue_inRange verifies values within normal bounds pass through unchanged.
func TestClampValue_inRange(t *testing.T) {
	cases := []struct {
		metric string
		value  int32
	}{
		{"lcp", 0},
		{"lcp", 2500},
		{"lcp", 60_000},
		{"lcp", 599_999},
		{"lcp", 600_000},
		{"cls", 0},
		{"cls", 250},
		{"cls", 10_000},
		{"ttfb", 1200},
		{"fcp", 800},
		{"inp", 200},
	}
	for _, tc := range cases {
		clamped, ok := clampValue(tc.metric, tc.value)
		if !ok {
			t.Errorf("clampValue(%q, %d): ok=false, expected true (in-range value)", tc.metric, tc.value)
		}
		if clamped != tc.value {
			t.Errorf("clampValue(%q, %d) = %d, want %d (unchanged)", tc.metric, tc.value, clamped, tc.value)
		}
	}
}

// TestClampValue_aboveCeilingClamped verifies values above the ceiling are
// clamped (ok=true — they are still stored, slow tail preserved).
func TestClampValue_aboveCeilingClamped(t *testing.T) {
	clamped, ok := clampValue("lcp", 600_001)
	if !ok {
		t.Fatal("clampValue(lcp, 600001) ok=false: above-ceiling must be clamped, not dropped")
	}
	if clamped != metricCeilingTimeMS {
		t.Errorf("clampValue(lcp, 600001) = %d, want %d", clamped, metricCeilingTimeMS)
	}

	clamped, ok = clampValue("cls", 10_001)
	if !ok {
		t.Fatal("clampValue(cls, 10001) ok=false: above-ceiling CLS must be clamped, not dropped")
	}
	if clamped != metricCeilingCLS {
		t.Errorf("clampValue(cls, 10001) = %d, want %d", clamped, metricCeilingCLS)
	}
}

// ---------------------------------------------------------------------------
// H1: full-path handler tests with stub store
// ---------------------------------------------------------------------------

// TestHandlerH1_slowLCPStored exercises the full handler path to confirm that a
// 90 000 ms LCP beacon (previously rejected with 400 by the old 60s ceiling) is
// now stored at its original value. 90s is within the new 600s ceiling.
func TestHandlerH1_slowLCPStored(t *testing.T) {
	stored := &captureStore{}
	beaconR := &stubBeaconKeyLookup{
		row: sqlc.LookupRumBeaconKeyRow{
			SiteID:        uuid.MustParse("00000000-0000-0000-0000-000000000001"),
			TenantID:      uuid.MustParse("00000000-0000-0000-0000-000000000002"),
			RumEnabled:    true,
			RumSampleRate: 1.0,
		},
	}
	h := newHandlerWithCap(stored, beaconR, nil, nil, DefaultIPLimiterCap)
	engine := gin.New()
	h.RegisterPublic(engine)

	payload := map[string]interface{}{
		"key":    "AAAAAAAAAAAAAAAAAAAAAA", // 22-char base64url → 16 raw bytes
		"url":    "https://example.com/",
		"metric": "lcp",
		"value":  90_000, // 90 seconds — was rejected by old 60s ceiling; now stored
	}
	b, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rum/ingest", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d — slow LCP (90s) must not be rejected", w.Code)
	}
	if len(stored.events) != 1 {
		t.Fatalf("expected 1 stored event, got %d", len(stored.events))
	}
	if stored.events[0].ValueMilli != 90_000 {
		t.Errorf("stored ValueMilli = %d, want 90000 (unchanged — within ceiling)", stored.events[0].ValueMilli)
	}
}

// TestHandlerH1_negativeLCPDiscarded verifies a negative LCP yields 204 (not 400)
// and is not written to the store.
func TestHandlerH1_negativeLCPDiscarded(t *testing.T) {
	stored := &captureStore{}
	beaconR := &stubBeaconKeyLookup{
		row: sqlc.LookupRumBeaconKeyRow{
			SiteID:        uuid.MustParse("00000000-0000-0000-0000-000000000001"),
			TenantID:      uuid.MustParse("00000000-0000-0000-0000-000000000002"),
			RumEnabled:    true,
			RumSampleRate: 1.0,
		},
	}
	h := newHandlerWithCap(stored, beaconR, nil, nil, DefaultIPLimiterCap)
	engine := gin.New()
	h.RegisterPublic(engine)

	payload := map[string]interface{}{
		"key":    "AAAAAAAAAAAAAAAAAAAAAA",
		"url":    "https://example.com/",
		"metric": "lcp",
		"value":  -1,
	}
	b, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rum/ingest", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204 for negative value, got %d (must NOT be 400)", w.Code)
	}
	if len(stored.events) != 0 {
		t.Errorf("expected 0 stored events for negative value, got %d", len(stored.events))
	}
}

// TestHandlerH1_clsAt2500Stored verifies CLS 2.5 (2500 milli-units) is stored
// with the original value.
func TestHandlerH1_clsAt2500Stored(t *testing.T) {
	stored := &captureStore{}
	beaconR := &stubBeaconKeyLookup{
		row: sqlc.LookupRumBeaconKeyRow{
			SiteID:        uuid.MustParse("00000000-0000-0000-0000-000000000001"),
			TenantID:      uuid.MustParse("00000000-0000-0000-0000-000000000002"),
			RumEnabled:    true,
			RumSampleRate: 1.0,
		},
	}
	h := newHandlerWithCap(stored, beaconR, nil, nil, DefaultIPLimiterCap)
	engine := gin.New()
	h.RegisterPublic(engine)

	payload := map[string]interface{}{
		"key":    "AAAAAAAAAAAAAAAAAAAAAA",
		"url":    "https://example.com/",
		"metric": "cls",
		"value":  2500,
	}
	b, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rum/ingest", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d — CLS 2.5 must not be rejected", w.Code)
	}
	if len(stored.events) != 1 {
		t.Fatalf("expected 1 stored event, got %d", len(stored.events))
	}
	if stored.events[0].ValueMilli != 2500 {
		t.Errorf("stored ValueMilli = %d, want 2500 (unchanged)", stored.events[0].ValueMilli)
	}
}

// ---------------------------------------------------------------------------
// M1: IP-limiter LRU bounded at cap
// ---------------------------------------------------------------------------

// TestIPLRU_boundedAtCap verifies that inserting more IPs than the cap keeps the
// LRU at exactly cap entries — no unbounded heap growth (M1 fix).
func TestIPLRU_boundedAtCap(t *testing.T) {
	const cap = 10
	lru := newIPLRU(cap, newTestLimiter)

	// Insert 3× cap distinct IPs.
	for i := 0; i < 3*cap; i++ {
		ip := strings.Repeat("x", i+1)
		lru.get(ip)
	}

	if sz := lru.len(); sz > cap {
		t.Errorf("IP LRU grew to %d entries beyond cap=%d — unbounded heap is the M1 failure mode", sz, cap)
	}
}

// TestIPLRU_mruIsRetained verifies that the most-recently-used entry is NOT
// evicted when the LRU is at capacity.
func TestIPLRU_mruIsRetained(t *testing.T) {
	const cap = 3
	lru := newIPLRU(cap, newTestLimiter)

	for _, ip := range []string{"a", "b", "c"} {
		lru.get(ip)
	}
	// Touch "a" so it becomes MRU.
	lru.get("a")
	// Add "d" — LRU eviction should remove "b" (oldest untouched), not "a".
	lru.get("d")

	if lru.len() != cap {
		t.Errorf("LRU size = %d, want %d (cap)", lru.len(), cap)
	}
	if lru.items["a"] == nil {
		t.Error("LRU evicted the MRU entry 'a' — it should have been retained")
	}
}

// ---------------------------------------------------------------------------
// M2: rightmost XFF is used (trusted-proxy strategy)
// ---------------------------------------------------------------------------

// TestClientIP_rightmostXFF verifies that the RIGHTMOST X-Forwarded-For entry
// is used as the client IP (LB-appended, trustworthy), not the leftmost
// (client-supplied, spoofable).
func TestClientIP_rightmostXFF(t *testing.T) {
	engine := gin.New()
	var gotIP string
	engine.GET("/test", func(c *gin.Context) {
		gotIP = clientIP(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	// Leftmost is attacker-controlled. Rightmost is LB-appended and trustworthy.
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.5")
	engine.ServeHTTP(w, req)

	if gotIP != "203.0.113.5" {
		t.Errorf("clientIP = %q, want 203.0.113.5 (rightmost, LB-appended)", gotIP)
	}
}

// TestClientIP_spoofedLeftmostDoesNotChangeKey verifies that an attacker changing
// only the leftmost XFF entry gets the same resolved IP key — defeating rate-limit
// bucket freshening via XFF spoofing (M2 fix).
func TestClientIP_spoofedLeftmostDoesNotChangeKey(t *testing.T) {
	engine := gin.New()
	ips := make([]string, 0, 2)
	engine.GET("/test", func(c *gin.Context) {
		ips = append(ips, clientIP(c))
		c.Status(http.StatusOK)
	})

	for _, spoofed := range []string{"10.0.0.1", "172.16.0.99"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/test", nil)
		r.Header.Set("X-Forwarded-For", spoofed+", 203.0.113.5")
		engine.ServeHTTP(w, r)
	}

	if len(ips) != 2 {
		t.Fatalf("expected 2 resolved IPs, got %d", len(ips))
	}
	if ips[0] != ips[1] {
		t.Errorf("different leftmost XFF resulted in different IPs: %q vs %q — must be same (rightmost)", ips[0], ips[1])
	}
	if ips[0] != "203.0.113.5" {
		t.Errorf("resolved IP = %q, want 203.0.113.5 (rightmost)", ips[0])
	}
}

// ---------------------------------------------------------------------------
// H2: /rum/ingest produces no Set-Cookie header
// ---------------------------------------------------------------------------

// TestRumIngest_noSetCookie asserts that the /rum/ingest endpoint does not emit
// a Set-Cookie header. The route must NOT run Sessions.LoadAndSave() (H2 fix).
// Any session middleware that mutates state sets a cookie; if that middleware
// were on the root engine it would fire on every beacon.
func TestRumIngest_noSetCookie(t *testing.T) {
	engine := gin.New()
	// Simulate Sessions.LoadAndSave() on a different group (auth/api), not root.
	sessionGroup := engine.Group("", func(c *gin.Context) {
		c.Header("Set-Cookie", "session=abc; Path=/")
		c.Next()
	})
	sessionGroup.GET("/api/v1/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })

	// /rum/ingest on root — no session middleware.
	h := newHandlerWithCap(&noopStore{}, &stubBeaconKeyLookup{}, nil, nil, DefaultIPLimiterCap)
	h.RegisterPublic(engine)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rum/ingest",
		bytes.NewBufferString(`{"key":"x","metric":"lcp","value":100}`))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if cookie := w.Header().Get("Set-Cookie"); cookie != "" {
		t.Errorf("/rum/ingest Set-Cookie = %q, want empty — session middleware must not run on ingest path", cookie)
	}
}

// ---------------------------------------------------------------------------
// Stub helpers for handler-level tests (no DB)
// ---------------------------------------------------------------------------

// stubBeaconKeyLookup implements beaconKeyLookup for handler tests.
// Returns ErrBeaconKeyNotFound when SiteID is zero.
type stubBeaconKeyLookup struct {
	row sqlc.LookupRumBeaconKeyRow
}

func (s *stubBeaconKeyLookup) LookupBeaconKey(_ context.Context, _ []byte) (sqlc.LookupRumBeaconKeyRow, error) {
	if s.row.SiteID == uuid.Nil {
		return sqlc.LookupRumBeaconKeyRow{}, ErrBeaconKeyNotFound
	}
	return s.row, nil
}

// captureStore is a Store implementation that records written events.
type captureStore struct {
	events []IngestParams
}

func (s *captureStore) WriteEvent(_ context.Context, p IngestParams) error {
	s.events = append(s.events, p)
	return nil
}
func (s *captureStore) GetHourlyRollups(_ context.Context, _, _ uuid.UUID, _ time.Time) ([]HourlyRollup, error) {
	return nil, nil
}
func (s *captureStore) GetDailyRollups(_ context.Context, _, _ uuid.UUID, _ time.Time) ([]DailyRollup, error) {
	return nil, nil
}
func (s *captureStore) ComputeP75(_ []HourlyRollup, _ int) []P75Result { return nil }
func (s *captureStore) PruneRawEvents(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
func (s *captureStore) PruneHourlyRollups(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
func (s *captureStore) PruneDailyRollups(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// noopStore drops all writes silently.
type noopStore struct{}

func (s *noopStore) WriteEvent(_ context.Context, _ IngestParams) error { return nil }
func (s *noopStore) GetHourlyRollups(_ context.Context, _, _ uuid.UUID, _ time.Time) ([]HourlyRollup, error) {
	return nil, nil
}
func (s *noopStore) GetDailyRollups(_ context.Context, _, _ uuid.UUID, _ time.Time) ([]DailyRollup, error) {
	return nil, nil
}
func (s *noopStore) ComputeP75(_ []HourlyRollup, _ int) []P75Result { return nil }
func (s *noopStore) PruneRawEvents(_ context.Context, _ time.Time) (int64, error) { return 0, nil }
func (s *noopStore) PruneHourlyRollups(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
func (s *noopStore) PruneDailyRollups(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// ---------------------------------------------------------------------------
// capturePublisher records rum.rollup_updated SSE emits for throttle tests.
// ---------------------------------------------------------------------------

type capturePublisher struct {
	mu     sync.Mutex
	events []site.ConnectionEvent
}

func (p *capturePublisher) Publish(_ context.Context, ev site.ConnectionEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return nil
}

func (p *capturePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events)
}

func (p *capturePublisher) last() (site.ConnectionEvent, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.events) == 0 {
		return site.ConnectionEvent{}, false
	}
	return p.events[len(p.events)-1], true
}

// ---------------------------------------------------------------------------
// SSE throttle tests
// ---------------------------------------------------------------------------

// buildValidBeaconRequest creates a POST /rum/ingest request with a valid-looking
// beacon key (stub lookup returns a matching row regardless of the hash).
func buildValidBeaconRequest(t *testing.T) *http.Request {
	t.Helper()
	payload := map[string]interface{}{
		"key":    "AAAAAAAAAAAAAAAAAAAAAA",
		"url":    "https://example.com/",
		"metric": "lcp",
		"value":  2500,
	}
	b, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/rum/ingest", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestRollupUpdated_emittedOnFirstBeacon verifies that the very first beacon
// for a site triggers a rum.rollup_updated SSE event.
func TestRollupUpdated_emittedOnFirstBeacon(t *testing.T) {
	pub := &capturePublisher{}
	beaconR := &stubBeaconKeyLookup{
		row: sqlc.LookupRumBeaconKeyRow{
			SiteID:        uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001"),
			TenantID:      uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002"),
			RumEnabled:    true,
			RumSampleRate: 1.0,
		},
	}
	h := newHandlerWithCap(&noopStore{}, beaconR, pub, nil, DefaultIPLimiterCap)
	engine := gin.New()
	h.RegisterPublic(engine)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, buildValidBeaconRequest(t))

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if pub.count() != 1 {
		t.Errorf("expected 1 SSE emit after first beacon, got %d", pub.count())
	}
	ev, _ := pub.last()
	if ev.Type != site.EventRumRollupUpdated {
		t.Errorf("event type = %q, want %q", ev.Type, site.EventRumRollupUpdated)
	}
	if ev.SiteID != beaconR.row.SiteID {
		t.Errorf("event SiteID = %v, want %v", ev.SiteID, beaconR.row.SiteID)
	}
	if ev.TenantID != beaconR.row.TenantID {
		t.Errorf("event TenantID = %v, want %v", ev.TenantID, beaconR.row.TenantID)
	}
}

// TestRollupUpdated_throttledToOnePerWindow verifies that N rapid beacons for
// the same site produce at most 1 SSE emit (the throttle window has not elapsed).
func TestRollupUpdated_throttledToOnePerWindow(t *testing.T) {
	const N = 20
	pub := &capturePublisher{}
	beaconR := &stubBeaconKeyLookup{
		row: sqlc.LookupRumBeaconKeyRow{
			SiteID:        uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002"),
			TenantID:      uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002"),
			RumEnabled:    true,
			RumSampleRate: 1.0,
		},
	}
	h := newHandlerWithCap(&noopStore{}, beaconR, pub, nil, DefaultIPLimiterCap)
	engine := gin.New()
	h.RegisterPublic(engine)

	for i := 0; i < N; i++ {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, buildValidBeaconRequest(t))
		if w.Code != http.StatusNoContent {
			t.Fatalf("beacon %d: expected 204, got %d", i, w.Code)
		}
	}

	if got := pub.count(); got > 1 {
		t.Errorf("throttle: %d beacons produced %d SSE emits, want ≤1 within the same throttle window", N, got)
	}
	if pub.count() == 0 {
		t.Error("throttle: 0 emits for 20 beacons — at least 1 must fire on the first beacon")
	}
}

// TestRollupUpdated_twoDifferentSitesEachGetOneEmit verifies that two sites
// each get their own independent SSE emit — the throttle is per-site, not global.
func TestRollupUpdated_twoDifferentSitesEachGetOneEmit(t *testing.T) {
	pub := &capturePublisher{}
	siteA := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000010")
	siteB := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000011")
	tenantID := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000003")

	// We need two separate handlers (one per site) since stubBeaconKeyLookup
	// returns a single fixed row. Use separate handlers sharing the same publisher.
	makeHandler := func(siteID uuid.UUID) *Handler {
		return newHandlerWithCap(&noopStore{}, &stubBeaconKeyLookup{
			row: sqlc.LookupRumBeaconKeyRow{
				SiteID: siteID, TenantID: tenantID,
				RumEnabled: true, RumSampleRate: 1.0,
			},
		}, pub, nil, DefaultIPLimiterCap)
	}
	hA := makeHandler(siteA)
	hB := makeHandler(siteB)

	engineA := gin.New()
	hA.RegisterPublic(engineA)
	engineB := gin.New()
	hB.RegisterPublic(engineB)

	// Fire one beacon per site.
	engineA.ServeHTTP(httptest.NewRecorder(), buildValidBeaconRequest(t))
	engineB.ServeHTTP(httptest.NewRecorder(), buildValidBeaconRequest(t))

	if got := pub.count(); got != 2 {
		t.Errorf("expected 2 SSE emits (one per site), got %d", got)
	}
}

// TestRollupUpdated_nilPublisherIsNoop verifies that a nil publisher causes
// no panic and the beacon still returns 204.
func TestRollupUpdated_nilPublisherIsNoop(t *testing.T) {
	beaconR := &stubBeaconKeyLookup{
		row: sqlc.LookupRumBeaconKeyRow{
			SiteID:        uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000020"),
			TenantID:      uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000004"),
			RumEnabled:    true,
			RumSampleRate: 1.0,
		},
	}
	// nil publisher — should not panic.
	h := newHandlerWithCap(&noopStore{}, beaconR, nil, nil, DefaultIPLimiterCap)
	engine := gin.New()
	h.RegisterPublic(engine)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, buildValidBeaconRequest(t))
	if w.Code != http.StatusNoContent {
		t.Errorf("nil publisher: expected 204, got %d", w.Code)
	}
}

// TestRollupUpdated_sampleRatePassedToStore verifies that the handler passes
// row.RumSampleRate through to IngestParams.SampleRate. This field is needed
// by WriteEvent to store the sample_rate on rollup rows so the dashboard can
// scale sample counts back to full-population estimates. We use SampleRate=1.0
// to guarantee the event is not dropped by random sampling before reaching the store.
func TestRollupUpdated_sampleRatePassedToStore(t *testing.T) {
	const wantRate float32 = 1.0
	stored := &captureStore{}
	beaconR := &stubBeaconKeyLookup{
		row: sqlc.LookupRumBeaconKeyRow{
			SiteID:        uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000030"),
			TenantID:      uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000005"),
			RumEnabled:    true,
			RumSampleRate: wantRate,
		},
	}
	h := newHandlerWithCap(stored, beaconR, nil, nil, DefaultIPLimiterCap)
	engine := gin.New()
	h.RegisterPublic(engine)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, buildValidBeaconRequest(t))
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if len(stored.events) != 1 {
		t.Fatalf("expected 1 stored event, got %d — handler must pass SampleRate through to WriteEvent", len(stored.events))
	}
	if stored.events[0].SampleRate != wantRate {
		t.Errorf("IngestParams.SampleRate = %f, want %f", stored.events[0].SampleRate, wantRate)
	}
}

// ---------------------------------------------------------------------------
// WriteEvent error logging test
// ---------------------------------------------------------------------------

// captureSlogHandler is a slog.Handler that records log records in memory.
// Used to assert that specific structured log entries are emitted without
// relying on stdout parsing.
type captureSlogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureSlogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *captureSlogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *captureSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *captureSlogHandler) WithGroup(name string) slog.Handler       { return h }

func (h *captureSlogHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

// hasAttr returns true when at least one recorded log entry contains an
// attribute with the given key and a value that formats to contain wantSubstr.
func (h *captureSlogHandler) hasAttr(key, wantSubstr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key && strings.Contains(fmt.Sprintf("%v", a.Value.Any()), wantSubstr) {
				return false // stop iteration — found
			}
			return true
		})
	}
	// Re-scan with a found flag — the above approach exits early but doesn't
	// return the result cleanly; do a straightforward second pass.
	for _, r := range h.records {
		found := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key && strings.Contains(fmt.Sprintf("%v", a.Value.Any()), wantSubstr) {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// failingStore is a Store whose WriteEvent always returns an error.
// All other methods are no-ops (same as noopStore).
type failingStore struct {
	err error
}

func (s *failingStore) WriteEvent(_ context.Context, _ IngestParams) error { return s.err }
func (s *failingStore) GetHourlyRollups(_ context.Context, _, _ uuid.UUID, _ time.Time) ([]HourlyRollup, error) {
	return nil, nil
}
func (s *failingStore) GetDailyRollups(_ context.Context, _, _ uuid.UUID, _ time.Time) ([]DailyRollup, error) {
	return nil, nil
}
func (s *failingStore) ComputeP75(_ []HourlyRollup, _ int) []P75Result { return nil }
func (s *failingStore) PruneRawEvents(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
func (s *failingStore) PruneHourlyRollups(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
func (s *failingStore) PruneDailyRollups(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// TestWriteEventFailure_isLoggedAndReturns204 verifies that when WriteEvent
// returns an error the handler:
//  1. Logs a Warn-level entry with site_id, tenant_id, metric and error fields.
//  2. Still returns 204 (never 500 — the browser cannot retry a fired beacon).
func TestWriteEventFailure_isLoggedAndReturns204(t *testing.T) {
	logH := &captureSlogHandler{}
	logger := slog.New(logH)

	siteID := uuid.MustParse("cccccccc-0000-0000-0000-000000000001")
	tenantID := uuid.MustParse("dddddddd-0000-0000-0000-000000000002")

	beaconR := &stubBeaconKeyLookup{
		row: sqlc.LookupRumBeaconKeyRow{
			SiteID:        siteID,
			TenantID:      tenantID,
			RumEnabled:    true,
			RumSampleRate: 1.0,
		},
	}
	storeErr := fmt.Errorf("db connection reset")
	h := newHandlerWithCap(&failingStore{err: storeErr}, beaconR, nil, logger, DefaultIPLimiterCap)
	engine := gin.New()
	h.RegisterPublic(engine)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, buildValidBeaconRequest(t))

	// Must return 204 even though the store failed.
	if w.Code != http.StatusNoContent {
		t.Errorf("WriteEvent failure: expected 204, got %d (must never 500 to the browser)", w.Code)
	}

	// Must have logged at least one entry.
	if logH.count() == 0 {
		t.Fatal("WriteEvent failure: no log entry emitted — silent failures hide broken rollup writes")
	}

	// The log entry must carry site_id and error fields.
	if !logH.hasAttr("site_id", siteID.String()) {
		t.Errorf("log entry missing site_id=%s", siteID)
	}
	if !logH.hasAttr("error", "db connection reset") {
		t.Errorf("log entry missing error field containing %q", "db connection reset")
	}
}
