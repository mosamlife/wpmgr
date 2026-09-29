package tests

// GH #755 Part B — push-time address adoption. Once a site is enrolled and
// connected, its DAILY diagnostics push (the "http" category's home_url) and
// its periodic metadata push (home_url) each queue the agent-reported address
// as a site_adopt_reported_url job, whose worker runs site.AdoptReportedURL
// to decide whether it replaces the saved one. The rule is
// siteaddr.PlanStrict: a leading "www." toggle and/or an http to https
// upgrade, on the same port and path, and ONLY after a signed ping confirms
// it when the job runs (adopt_url.go).
//
// The env queues the job in gh755AdoptQueue, and pushDiagnostics and
// pushMetadata run each queued job through the real worker as soon as the
// push returns, so every case below reads the outcome the job decides.
// TestGH755Push_AdoptionNeverHoldsThePush pins that the push itself never
// waits for the probe, and TestGH755Push_AdoptionJobRunsOnRiver runs the job
// on a real River client.
//
// Every request here goes through the REAL mounted routes (POST /enroll,
// POST /sites, POST /agent/v1/diagnostics, POST /agent/v1/metadata, each
// behind its real production middleware — the agent Ed25519 signed-request
// Authenticator for the two pushes), against a database reached as
// wpmgr_app (NOSUPERUSER, NOBYPASSRLS), which gh755AssertAppRole checks
// rather than assumes. The signed ping itself is answered by a test double
// (gh755FakeProber) implementing site.CommandRedirectProber: never an
// external host.
//
// Every #755 integration test, in this file and in
// gh755_enroll_address_integration_test.go, is selected by exactly one regex:
//
//	^(TestSiteFirstEnroll_|TestReEnroll_|TestReEnrollAddressGate_|TestSiteMint_|TestGH755Push_)
//
// `go -C apps/api test -list '<that regex>' ./tests/` must list every
// TestSiteFirstEnroll_*, TestReEnroll_*, TestReEnrollAddressGate_*,
// TestSiteMint_* and TestGH755Push_* function and nothing else.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/diagnostics"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// gh755AdoptQueue is the env's site.AdoptURLEnqueuer: it records each job a
// push queues, or refuses it when err is set. runAdoptJobs runs the recorded
// jobs through the real worker.
type gh755AdoptQueue struct {
	mu   sync.Mutex
	jobs []site.AdoptReportedURLArgs
	err  error
}

func (q *gh755AdoptQueue) EnqueueAdoptURL(_ context.Context, a site.AdoptReportedURLArgs) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	q.jobs = append(q.jobs, a)
	return nil
}

// take removes and returns every queued job.
func (q *gh755AdoptQueue) take() []site.AdoptReportedURLArgs {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.jobs
	q.jobs = nil
	return out
}

// redirectAnswer is one canned answer to CommandRedirectTarget.
type redirectAnswer struct {
	suggested  string
	redirected bool
}

// gh755FakeProber answers site.CommandRedirectProber from a table the test
// configures, and records every call. beforeAnswer, when set, runs
// synchronously just before an answer is returned — used to inject a
// concurrent write between AdoptReportedURL's read and its compare-and-set
// write (the "stale from" case) without a real race.
type gh755FakeProber struct {
	mu            sync.Mutex
	pingOK        map[string]bool
	redirectTo    map[string]redirectAnswer
	beforeAnswer  func()
	pingCalls     []string
	redirectCalls []string
}

func newGH755FakeProber() *gh755FakeProber {
	return &gh755FakeProber{pingOK: map[string]bool{}, redirectTo: map[string]redirectAnswer{}}
}

func (p *gh755FakeProber) allowPing(url string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pingOK[url] = true
}

func (p *gh755FakeProber) setRedirect(savedURL, suggested string, redirected bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.redirectTo[savedURL] = redirectAnswer{suggested: suggested, redirected: redirected}
}

func (p *gh755FakeProber) CommandRedirectTarget(_ context.Context, _ uuid.UUID, siteURL string) (string, bool, bool) {
	p.mu.Lock()
	p.redirectCalls = append(p.redirectCalls, siteURL)
	ans := p.redirectTo[siteURL]
	hook := p.beforeAnswer
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	return ans.suggested, ans.redirected, true
}

func (p *gh755FakeProber) CommandPingOK(_ context.Context, _ uuid.UUID, siteURL string) (bool, bool) {
	p.mu.Lock()
	p.pingCalls = append(p.pingCalls, siteURL)
	ok := p.pingOK[siteURL]
	hook := p.beforeAnswer
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	return ok, true
}

func (p *gh755FakeProber) calls(t *testing.T) (ping, redirect int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pingCalls), len(p.redirectCalls)
}

// pingsSince returns the addresses CommandPingOK was called with after the
// first n calls.
func (p *gh755FakeProber) pingsSince(n int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.pingCalls[n:]...)
}

// gh755PushEnv is one control plane mounted the way server.go mounts it: the
// public /enroll and the authed site routes under /api/v1 (for "Add site"),
// PLUS the real agent-authenticated /agent/v1 group carrying the diagnostics
// and metadata pushes — all on one engine, all against the same pool.
type gh755PushEnv struct {
	pool   *db.Pool
	eng    *gin.Engine
	svc    *site.Service
	rec    *audit.Recorder
	prober *gh755FakeProber
	adoptQ *gh755AdoptQueue
	as     domain.Principal
}

func newGH755PushEnv(t *testing.T, pool *db.Pool) *gh755PushEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := site.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	conn := site.NewConnectionService(repo, domain.NewValidator(), rec, nil, domain.SystemClock{}, nil)
	svc := site.NewService(repo, domain.NewValidator(), domain.SystemClock{})
	svc.SetConnectionService(conn)
	svc.SetAuditRecorder(rec)
	prober := newGH755FakeProber()
	svc.SetCommandRedirectProber(prober)
	adoptQ := &gh755AdoptQueue{}
	svc.SetAdoptURLEnqueuer(adoptQ)

	diagSvc := diagnostics.NewService(diagnostics.NewRepo(pool))
	diagSvc.SetReportedURLSink(svc)

	h := site.NewHandler(svc, rec, "")
	h.SetConnectionService(conn)
	agentH := agent.NewHandler(svc)
	diagAgentH := agent.NewDiagnosticsHandler(diagSvc)
	authn := agent.NewAuthenticator(svc, domain.SystemClock{}, 5*time.Minute)

	env := &gh755PushEnv{pool: pool, svc: svc, rec: rec, prober: prober, adoptQ: adoptQ}
	eng := gin.New()
	h.RegisterPublic(eng)
	v1 := eng.Group("/api/v1")
	v1.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), env.as))
		c.Next()
	})
	h.Register(v1)
	ag := eng.Group("/agent/v1")
	ag.Use(authn.Authenticate())
	agentH.Register(ag)
	diagAgentH.Register(ag)
	env.eng = eng
	return env
}

func (e *gh755PushEnv) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.10:4444"
	w := httptest.NewRecorder()
	e.eng.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// addSite runs the dashboard's "Add site" (POST /sites) as e.as, returning
// the new site id and its enrollment code.
func (e *gh755PushEnv) addSite(t *testing.T, url string) (uuid.UUID, string) {
	t.Helper()
	code, body := e.do(t, http.MethodPost, "/api/v1/sites", map[string]any{"url": url, "name": url})
	if code != http.StatusCreated {
		t.Fatalf("POST /sites %s answered %d, want 201: %v", url, code, body)
	}
	id, err := uuid.Parse(body["site_id"].(string))
	if err != nil {
		t.Fatalf("site_id: %v", err)
	}
	return id, body["enrollment_code"].(string)
}

// enroll is the agent's POST /enroll, reporting reportedURL as its home_url.
// It returns the agent's Ed25519 keypair so the caller can sign later pushes.
func (e *gh755PushEnv) enroll(t *testing.T, pairingCode, reportedURL string, want uuid.UUID) (ed25519.PrivateKey, string) {
	t.Helper()
	_, priv, pub := genKey(t)
	code, body := e.do(t, http.MethodPost, "/enroll", map[string]any{
		"pairing_code":     pairingCode,
		"site_url":         reportedURL,
		"agent_public_key": pub,
		"wp_version":       "6.6",
		"php_version":      "8.3",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /enroll reporting %s answered %d, want 200: %v", reportedURL, code, body)
	}
	if body["site_id"] != want.String() {
		t.Fatalf("enroll attached to %v, want %s", body["site_id"], want)
	}
	return priv, pub
}

// connectSite mints a site, enrolls it reporting the SAME address it was
// added with (so enrollment adopts nothing and the site starts connected at
// exactly url), and returns its id and the agent's signing key.
func (e *gh755PushEnv) connectSite(t *testing.T, url string) (uuid.UUID, ed25519.PrivateKey, string) {
	t.Helper()
	id, code := e.addSite(t, url)
	priv, pub := e.enroll(t, code, url, id)
	return id, priv, pub
}

// signedPush signs body the way the agent plugin does (agent.CanonicalMessage)
// and POSTs it to an /agent/v1 route through the real Authenticator.
func (e *gh755PushEnv) signedPush(t *testing.T, priv ed25519.PrivateKey, pub, path string, body []byte) (int, map[string]any) {
	t.Helper()
	tsStr := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "gh755-push-" + uuid.NewString()
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, agent.CanonicalMessage(http.MethodPost, path, tsStr, nonce, body)))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(agent.HeaderAgentKey, pub)
	req.Header.Set(agent.HeaderTimestamp, tsStr)
	req.Header.Set(agent.HeaderNonce, nonce)
	req.Header.Set(agent.HeaderSignature, sig)
	w := httptest.NewRecorder()
	e.eng.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// diagnosticsBody is the agent's daily 14-category push body, carrying only
// the "http" category's home_url (the only field AdoptReportedURL reads).
func diagnosticsBody(t *testing.T, homeURL string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"http":         map[string]any{"home_url": homeURL},
		"collected_at": time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("marshal diagnostics body: %v", err)
	}
	return body
}

// metadataBody is the agent's periodic metadata push body, carrying home_url
// and, when it is not empty, agent_version.
func metadataBody(t *testing.T, homeURL, agentVersion string) []byte {
	t.Helper()
	m := map[string]any{"wp_version": "6.6", "home_url": homeURL}
	if agentVersion != "" {
		m["agent_version"] = agentVersion
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal metadata body: %v", err)
	}
	return body
}

// pushDiagnostics is the agent's diagnostics push, followed by every
// adoption job it queued, run through the real worker.
func (e *gh755PushEnv) pushDiagnostics(t *testing.T, priv ed25519.PrivateKey, pub, homeURL string) (int, map[string]any) {
	t.Helper()
	status, out := e.signedPush(t, priv, pub, "/agent/v1/diagnostics", diagnosticsBody(t, homeURL))
	e.runAdoptJobs(t)
	return status, out
}

// pushMetadata is the agent's metadata push, followed by every adoption job
// it queued, run through the real worker.
func (e *gh755PushEnv) pushMetadata(t *testing.T, priv ed25519.PrivateKey, pub, homeURL string) (int, map[string]any) {
	t.Helper()
	status, out := e.signedPush(t, priv, pub, "/agent/v1/metadata", metadataBody(t, homeURL, ""))
	e.runAdoptJobs(t)
	return status, out
}

// runAdoptJobs runs every queued adoption job through the worker production
// registers with River. A job error fails the test: River would retry it.
func (e *gh755PushEnv) runAdoptJobs(t *testing.T) {
	t.Helper()
	w := site.NewAdoptReportedURLWorker(e.svc)
	for _, a := range e.adoptQ.take() {
		if err := w.Work(context.Background(), &river.Job[site.AdoptReportedURLArgs]{Args: a}); err != nil {
			t.Errorf("adoption job for %s: %v", a.Reported, err)
		}
	}
}

func (e *gh755PushEnv) site(t *testing.T, tenant, id uuid.UUID) site.Site {
	t.Helper()
	s, err := e.svc.Get(context.Background(), tenant, id)
	if err != nil {
		t.Fatalf("get site %s: %v", id, err)
	}
	return s
}

// urlAudits returns the site.url_changed and site.url_mismatch rows for one
// site, read through the recorder's own tenant-scoped List (mirrors
// gh755Env.urlAudits in gh755_enroll_address_integration_test.go).
func (e *gh755PushEnv) urlAudits(t *testing.T, tenant, siteID uuid.UUID) (changed, mismatch []audit.Entry) {
	t.Helper()
	entries, err := e.rec.List(context.Background(), tenant, 500, 0)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	for _, en := range entries {
		if en.TargetType != "site" || en.TargetID != siteID.String() {
			continue
		}
		switch en.Action {
		case audit.ActionSiteURLChanged:
			changed = append(changed, en)
		case audit.ActionSiteURLMismatch:
			mismatch = append(mismatch, en)
		}
	}
	return changed, mismatch
}

// assertPushURLChanged checks a site.url_changed row produced by a PUSH
// (diagnostics or metadata), whose source is agent_diagnostics or
// agent_metadata — unlike the enrollment-path rows assertURLChanged expects
// (source always agent_enrollment), so it cannot be reused here.
func assertPushURLChanged(t *testing.T, got []audit.Entry, from, to, source string) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("site.url_changed rows = %d, want 1", len(got))
	}
	m := got[0].Metadata
	if m["from"] != from || m["to"] != to || m["source"] != source {
		t.Fatalf("site.url_changed metadata = %v, want from=%s to=%s source=%s", m, from, to, source)
	}
	if got[0].ActorType != audit.ActorSystem {
		t.Fatalf("site.url_changed actor = %s, want %s", got[0].ActorType, audit.ActorSystem)
	}
}

func gh755PushSetup(t *testing.T, slug string) (*gh755PushEnv, uuid.UUID) {
	t.Helper()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, slug)
	gh755AssertAppRole(t, pool, tenant)
	env := newGH755PushEnv(t, pool)
	env.as = gh755Owner(tenant, gh755SeedUser(t, pool, slug+"@example.test"))
	return env, tenant
}

// execTenantSQL runs a statement inside the tenant's own RLS scope (the same
// tx helper the request path uses) — used to inject a raw address change
// mid-probe (the "stale from" case) and to seed a duplicate-address holder
// under a live transaction (the race case).
func execTenantSQL(t *testing.T, pool *db.Pool, tenant uuid.UUID, sql string, args ...any) {
	t.Helper()
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	})
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// ---------------------------------------------------------------------------
// (a) http saved, https reported, same host: gated on a successful https ping.
// ---------------------------------------------------------------------------

func TestGH755Push_HTTPSUpgradeGatedOnPing(t *testing.T) {
	env, tenant := gh755PushSetup(t, "gh755p-https")

	t.Run("ping succeeds: adopted via the diagnostics push", func(t *testing.T) {
		id, priv, pub := env.connectSite(t, "http://pa1.example.com")
		env.prober.allowPing("https://pa1.example.com")

		status, body := env.pushDiagnostics(t, priv, pub, "https://pa1.example.com/")
		if status != http.StatusOK {
			t.Fatalf("diagnostics push answered %d, want 200: %v", status, body)
		}
		s := env.site(t, tenant, id)
		if s.URL != "https://pa1.example.com" {
			t.Fatalf("site url = %q, want the https address adopted", s.URL)
		}
		changed, mismatch := env.urlAudits(t, tenant, id)
		assertPushURLChanged(t, changed, "http://pa1.example.com", "https://pa1.example.com", "agent_diagnostics")
		if len(mismatch) != 0 {
			t.Fatalf("unexpected mismatch rows: %v", mismatch)
		}
	})

	t.Run("ping fails: not adopted via the metadata push", func(t *testing.T) {
		id, priv, pub := env.connectSite(t, "http://pa2.example.com")
		// No allowPing call: the fake prober answers false by default.

		status, body := env.pushMetadata(t, priv, pub, "https://pa2.example.com")
		if status != http.StatusOK {
			t.Fatalf("metadata push answered %d, want 200: %v", status, body)
		}
		s := env.site(t, tenant, id)
		if s.URL != "http://pa2.example.com" {
			t.Fatalf("site url = %q, want it unchanged after a failed ping", s.URL)
		}
		changed, mismatch := env.urlAudits(t, tenant, id)
		if len(changed) != 0 || len(mismatch) != 0 {
			t.Fatalf("a failed ping was audited: changed=%v mismatch=%v", changed, mismatch)
		}
	})
}

// ---------------------------------------------------------------------------
// (b) apex saved, www reported: gated on the SAVED address redirecting to
// exactly that www address.
// ---------------------------------------------------------------------------

func TestGH755Push_WWWHostChangeGatedOnRedirect(t *testing.T) {
	env, tenant := gh755PushSetup(t, "gh755p-www")

	t.Run("redirects to exactly the planned address: adopted", func(t *testing.T) {
		id, priv, pub := env.connectSite(t, "https://pb1.example.com")
		env.prober.setRedirect("https://pb1.example.com", "https://www.pb1.example.com", true)

		status, body := env.pushMetadata(t, priv, pub, "https://www.pb1.example.com")
		if status != http.StatusOK {
			t.Fatalf("metadata push answered %d, want 200: %v", status, body)
		}
		s := env.site(t, tenant, id)
		if s.URL != "https://www.pb1.example.com" {
			t.Fatalf("site url = %q, want the www address adopted", s.URL)
		}
		changed, _ := env.urlAudits(t, tenant, id)
		assertPushURLChanged(t, changed, "https://pb1.example.com", "https://www.pb1.example.com", "agent_metadata")
	})

	t.Run("does not redirect: not adopted", func(t *testing.T) {
		id, priv, pub := env.connectSite(t, "https://pb2.example.com")
		// No setRedirect call: the fake prober answers redirected=false.

		status, _ := env.pushDiagnostics(t, priv, pub, "https://www.pb2.example.com")
		if status != http.StatusOK {
			t.Fatalf("diagnostics push status = %d, want 200", status)
		}
		s := env.site(t, tenant, id)
		if s.URL != "https://pb2.example.com" {
			t.Fatalf("site url = %q, want it unchanged with no redirect", s.URL)
		}
		changed, mismatch := env.urlAudits(t, tenant, id)
		if len(changed)+len(mismatch) != 0 {
			t.Fatalf("no-redirect push was audited: changed=%v mismatch=%v", changed, mismatch)
		}
	})

	t.Run("redirects elsewhere: not adopted", func(t *testing.T) {
		id, priv, pub := env.connectSite(t, "https://pb3.example.com")
		env.prober.setRedirect("https://pb3.example.com", "https://attacker-pb3.example.net", true)

		status, _ := env.pushDiagnostics(t, priv, pub, "https://www.pb3.example.com")
		if status != http.StatusOK {
			t.Fatalf("diagnostics push status = %d, want 200", status)
		}
		s := env.site(t, tenant, id)
		if s.URL != "https://pb3.example.com" {
			t.Fatalf("site url = %q, want it unchanged when the redirect names another address", s.URL)
		}
		changed, mismatch := env.urlAudits(t, tenant, id)
		if len(changed)+len(mismatch) != 0 {
			t.Fatalf("wrong-target redirect was audited: changed=%v mismatch=%v", changed, mismatch)
		}
	})
}

// ---------------------------------------------------------------------------
// (c) not equivalent (another host, port or path): never adopted, no probe,
// no audit row (repeating every push would be noise).
// ---------------------------------------------------------------------------

func TestGH755Push_NotEquivalentNeverAdopted(t *testing.T) {
	env, tenant := gh755PushSetup(t, "gh755p-mismatch")

	cases := []struct {
		name, stored, reported string
	}{
		{"other host", "https://pc-host.example.com", "https://attacker-pc.example.net"},
		{"other port", "https://pc-port.example.com", "https://pc-port.example.com:8443"},
		{"other path", "https://pc-path.example.com", "https://pc-path.example.com/blog"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, priv, pub := env.connectSite(t, c.stored)
			// Even a prober that would say yes to everything must never be
			// asked: the decision is Mismatch before any probe.
			env.prober.allowPing(c.reported)
			env.prober.setRedirect(c.stored, c.reported, true)
			before, beforeR := env.prober.calls(t)

			status, _ := env.pushMetadata(t, priv, pub, c.reported)
			if status != http.StatusOK {
				t.Fatalf("metadata push status = %d, want 200", status)
			}
			s := env.site(t, tenant, id)
			if s.URL != c.stored {
				t.Fatalf("case %d site url = %q, want it unchanged", i, s.URL)
			}
			changed, mismatch := env.urlAudits(t, tenant, id)
			if len(changed)+len(mismatch) != 0 {
				t.Fatalf("case %d: a non-equivalent push was audited: changed=%v mismatch=%v", i, changed, mismatch)
			}
			after, afterR := env.prober.calls(t)
			if after != before || afterR != beforeR {
				t.Fatalf("case %d: probe was called for a non-equivalent address (ping %d->%d, redirect %d->%d)", i, before, after, beforeR, afterR)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (d) the same address: no write, no audit row, no probe.
// ---------------------------------------------------------------------------

func TestGH755Push_SameAddressIsNoOp(t *testing.T) {
	env, tenant := gh755PushSetup(t, "gh755p-same")
	id, priv, pub := env.connectSite(t, "https://pd1.example.com")
	before, beforeR := env.prober.calls(t)

	status, _ := env.pushDiagnostics(t, priv, pub, "https://PD1.example.com:443/")
	if status != http.StatusOK {
		t.Fatalf("diagnostics push status = %d, want 200", status)
	}
	s := env.site(t, tenant, id)
	if s.URL != "https://pd1.example.com" {
		t.Fatalf("site url = %q, want it unchanged", s.URL)
	}
	changed, mismatch := env.urlAudits(t, tenant, id)
	if len(changed)+len(mismatch) != 0 {
		t.Fatalf("an equal address was audited: changed=%v mismatch=%v", changed, mismatch)
	}
	after, afterR := env.prober.calls(t)
	if after != before || afterR != beforeR {
		t.Fatalf("an equal address was probed (ping %d->%d, redirect %d->%d)", before, after, beforeR, afterR)
	}
}

// ---------------------------------------------------------------------------
// (e) another site in the tenant holds the target: not adopted, and no
// oracle — the pre-existing-holder (no-rows) path and the commits-during-the-
// push (23505) path must look identical to the caller.
// ---------------------------------------------------------------------------

func TestGH755Push_DuplicateHolderNotAdoptedNoOracle(t *testing.T) {
	env, tenant := gh755PushSetup(t, "gh755p-holder")
	repo := site.NewRepo(env.pool)
	ctx := context.Background()

	t.Run("holder already committed (no-rows path)", func(t *testing.T) {
		id, priv, pub := env.connectSite(t, "https://pe1.example.com")
		if _, err := repo.CreatePending(ctx, tenant, "https://www.pe1.example.com", "holder-pe1", nil); err != nil {
			t.Fatalf("seed holder: %v", err)
		}
		env.prober.setRedirect("https://pe1.example.com", "https://www.pe1.example.com", true)

		status, body := env.pushDiagnostics(t, priv, pub, "https://www.pe1.example.com")
		if status != http.StatusOK {
			t.Fatalf("diagnostics push answered %d, want 200: %v", status, body)
		}
		if s := env.site(t, tenant, id); s.URL != "https://pe1.example.com" {
			t.Fatalf("site url = %q, want it unchanged", s.URL)
		}
		changed, mismatch := env.urlAudits(t, tenant, id)
		if len(changed)+len(mismatch) != 0 {
			t.Fatalf("a duplicate-address push was audited: changed=%v mismatch=%v", changed, mismatch)
		}
		if _, redirectCalls := env.prober.calls(t); redirectCalls == 0 {
			t.Fatal("the redirect probe was never called; this case must reach the write and fail there, not be refused earlier")
		}
	})

	t.Run("holder commits during the push (23505 path)", func(t *testing.T) {
		id, priv, pub := env.connectSite(t, "https://pe2.example.com")
		env.prober.setRedirect("https://pe2.example.com", "https://www.pe2.example.com", true)

		// Insert the holder in an open transaction so the push's own
		// conflict check (the NOT EXISTS guard) cannot see it, and its write
		// waits on the unique index instead.
		inserted := make(chan struct{})
		release := make(chan struct{})
		holderErr := make(chan error, 1)
		go func() {
			holderErr <- env.pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx,
					`INSERT INTO sites (tenant_id, url, name, status, connection_state, tags) VALUES ($1, $2, $3, 'pending', 'pending_enrollment', $4)`,
					tenant, "https://www.pe2.example.com", "holder-pe2", []string{}); err != nil {
					close(inserted)
					return err
				}
				close(inserted)
				<-release
				return nil
			})
		}()
		<-inserted

		var wg sync.WaitGroup
		var status int
		var body map[string]any
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body = env.pushDiagnostics(t, priv, pub, "https://www.pe2.example.com")
		}()

		// Bounded wait for the push's UPDATE to block on the holder's open tx.
		blocked := false
		for i := 0; i < 100 && !blocked; i++ {
			var n int
			if err := env.pool.QueryRow(ctx,
				`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query ILIKE '%UPDATE sites%'`).Scan(&n); err != nil {
				t.Fatalf("pg_stat_activity: %v", err)
			}
			blocked = n > 0
			if !blocked {
				time.Sleep(50 * time.Millisecond)
			}
		}
		close(release)
		wg.Wait()
		if err := <-holderErr; err != nil {
			t.Fatalf("holder insert: %v", err)
		}
		if !blocked {
			t.Fatal("the push never waited on the holder's transaction, so this case did not exercise the 23505 path")
		}

		if status != http.StatusOK {
			t.Fatalf("diagnostics push answered %d, want 200: %v", status, body)
		}
		if s := env.site(t, tenant, id); s.URL != "https://pe2.example.com" {
			t.Fatalf("site url = %q, want it unchanged", s.URL)
		}
		changed, mismatch := env.urlAudits(t, tenant, id)
		if len(changed)+len(mismatch) != 0 {
			t.Fatalf("a raced duplicate-address push was audited: changed=%v mismatch=%v", changed, mismatch)
		}
	})
}

// ---------------------------------------------------------------------------
// (f) a stale "from" (the address changed concurrently): not adopted.
// ---------------------------------------------------------------------------

func TestGH755Push_StaleFromNotAdopted(t *testing.T) {
	env, tenant := gh755PushSetup(t, "gh755p-stale")
	id, priv, pub := env.connectSite(t, "http://pf1.example.com")

	// The ping succeeds, but firing it is used as the injection point for a
	// concurrent write that changes the row AdoptReportedURL already read —
	// deterministic in place of a real goroutine race.
	env.prober.allowPing("https://pf1.example.com")
	env.prober.beforeAnswer = func() {
		execTenantSQL(t, env.pool, tenant,
			`UPDATE sites SET url = $1, updated_at = now() WHERE tenant_id = $2 AND id = $3`,
			"http://pf1-changed-concurrently.example.com", tenant, id)
	}

	status, body := env.pushDiagnostics(t, priv, pub, "https://pf1.example.com")
	if status != http.StatusOK {
		t.Fatalf("diagnostics push answered %d, want 200: %v", status, body)
	}
	s := env.site(t, tenant, id)
	if s.URL != "http://pf1-changed-concurrently.example.com" {
		t.Fatalf("site url = %q, want the concurrent write to have won (stale compare-and-set refused)", s.URL)
	}
	changed, mismatch := env.urlAudits(t, tenant, id)
	if len(changed)+len(mismatch) != 0 {
		t.Fatalf("a stale-from push was audited: changed=%v mismatch=%v", changed, mismatch)
	}
}

// ---------------------------------------------------------------------------
// (g) exactly one site.url_changed audit row per adoption, actor system, with
// source agent_diagnostics or agent_metadata — asserted inline in (a) and
// (b) above for both push channels; see assertURLChanged's len(got)==1 check
// (shared with gh755_enroll_address_integration_test.go) and the explicit
// source/actor assertions in the "ping succeeds" and "redirects to exactly
// the planned address" subtests.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// (h) cross-tenant: a push for tenant A can never change tenant B's site.
// ---------------------------------------------------------------------------

func TestGH755Push_CrossTenantNeverChangesOtherTenantsSite(t *testing.T) {
	envA, tenantA := gh755PushSetup(t, "gh755p-xtenant-a")
	pool := envA.pool
	tenantB := seedTenant(t, pool, "gh755p-xtenant-b")
	envB := newGH755PushEnv(t, pool)
	envB.as = gh755Owner(tenantB, gh755SeedUser(t, pool, "gh755p-xtenant-b@example.test"))

	// Both tenants independently enroll a site at the SAME starting address
	// (allowed: sites_tenant_id_url_key is scoped per tenant).
	idA, privA, pubA := envA.connectSite(t, "https://ph1.example.com")
	idB, _, _ := envB.connectSite(t, "https://ph1.example.com")

	envA.prober.setRedirect("https://ph1.example.com", "https://www.ph1.example.com", true)
	status, body := envA.pushDiagnostics(t, privA, pubA, "https://www.ph1.example.com")
	if status != http.StatusOK {
		t.Fatalf("tenant A diagnostics push answered %d, want 200: %v", status, body)
	}

	// Tenant A's site adopted the www address.
	sA := envA.site(t, tenantA, idA)
	if sA.URL != "https://www.ph1.example.com" {
		t.Fatalf("tenant A site url = %q, want the www address adopted", sA.URL)
	}
	changedA, _ := envA.urlAudits(t, tenantA, idA)
	assertPushURLChanged(t, changedA, "https://ph1.example.com", "https://www.ph1.example.com", "agent_diagnostics")

	// Tenant B's identically-addressed site is completely untouched: same
	// URL, same connection state, and no audit row of its own — read directly
	// under tenant B's own RLS scope, never through tenant A's principal.
	var url, state string
	err := pool.InTenantTx(context.Background(), tenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT url, connection_state FROM sites WHERE tenant_id = $1 AND id = $2`, tenantB, idB).Scan(&url, &state)
	})
	if err != nil {
		t.Fatalf("read tenant B's site: %v", err)
	}
	if url != "https://ph1.example.com" || state != string(site.StateConnected) {
		t.Fatalf("tenant B site url=%q state=%q, want it unchanged at the apex address", url, state)
	}
	changedB, mismatchB := envB.urlAudits(t, tenantB, idB)
	if len(changedB)+len(mismatchB) != 0 {
		t.Fatalf("tenant A's push left an audit trail on tenant B's site: changed=%v mismatch=%v", changedB, mismatchB)
	}
}

// ---------------------------------------------------------------------------
// (i) the reporter's trailing-slash shape: the saved address itself carries a
// trailing slash, that saved address redirects to the www form, and the push
// reports the www form. Adopted, keeping the saved address's path (the
// slash, per siteaddr.Plan's "To is built from the stored address" rule),
// with exactly one url_changed row.
// ---------------------------------------------------------------------------

func TestGH755Push_TrailingSlashSavedRedirectsToWWW(t *testing.T) {
	env, tenant := gh755PushSetup(t, "gh755p-slash")
	id, priv, pub := env.connectSite(t, "https://slash1.example.com/")
	env.prober.setRedirect("https://slash1.example.com/", "https://www.slash1.example.com", true)

	status, body := env.pushMetadata(t, priv, pub, "https://www.slash1.example.com")
	if status != http.StatusOK {
		t.Fatalf("metadata push answered %d, want 200: %v", status, body)
	}
	s := env.site(t, tenant, id)
	if s.URL != "https://www.slash1.example.com/" {
		t.Fatalf("site url = %q, want the www address adopted with the saved trailing slash kept", s.URL)
	}
	changed, mismatch := env.urlAudits(t, tenant, id)
	assertPushURLChanged(t, changed, "https://slash1.example.com/", "https://www.slash1.example.com/", "agent_metadata")
	if len(mismatch) != 0 {
		t.Fatalf("unexpected mismatch rows: %v", mismatch)
	}
}

// ---------------------------------------------------------------------------
// (j) a host that differs from the saved one only by Unicode-vs-ASCII casing
// of a non-ASCII letter (a dotted capital İ, a capital ẞ) is never adopted at
// push time, and reaches no probe: siteaddr.PlanStrict compares hosts by
// HostKey (ASCII letters lowercased, IDNA only for a non-ASCII host), never
// Unicode case folding, so these name two different hosts, not a case
// variant of the same one, and Plan's own host check refuses before any
// probe would fire.
// ---------------------------------------------------------------------------

func TestGH755Push_UnicodeCaseHostNeverAdopted(t *testing.T) {
	env, tenant := gh755PushSetup(t, "gh755p-unicode")

	cases := []struct {
		name, stored, reported string
	}{
		{"dotted capital I", "http://İstanbul.example.test", "https://istanbul.example.test"},
		{"capital sharp S", "http://STRAẞE.example.test", "https://straße.example.test"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, priv, pub := env.connectSite(t, c.stored)
			// A prober that would say yes to everything must never be asked:
			// the decision is Mismatch before any probe.
			env.prober.allowPing(c.reported)
			env.prober.setRedirect(c.stored, c.reported, true)
			before, beforeR := env.prober.calls(t)

			status, body := env.pushDiagnostics(t, priv, pub, c.reported)
			if status != http.StatusOK {
				t.Fatalf("diagnostics push answered %d, want 200: %v", status, body)
			}
			s := env.site(t, tenant, id)
			if s.URL != c.stored {
				t.Fatalf("site url = %q, want it unchanged", s.URL)
			}
			changed, mismatch := env.urlAudits(t, tenant, id)
			if len(changed)+len(mismatch) != 0 {
				t.Fatalf("a Unicode-case host push was audited: changed=%v mismatch=%v", changed, mismatch)
			}
			after, afterR := env.prober.calls(t)
			if after != before || afterR != beforeR {
				t.Fatalf("probe was called for a Unicode-case host (ping %d->%d, redirect %d->%d)", before, after, beforeR, afterR)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (k) a site saved over http with a host whose Unicode lowercase is another
// domain, whose agent pushes the same host over https: AdoptReportedURL
// pings, and stores, the https address of the saved host, which dials the
// saved host's key, never the host Unicode lowercasing gives. The fake
// prober answers the ping for both, so only the address rule decides.
// ---------------------------------------------------------------------------

func TestGH755Push_UnicodeHostUpgradeStoresTheSavedHost(t *testing.T) {
	env, tenant := gh755PushSetup(t, "gh755p-unicode-up")

	for _, c := range gh755UnicodeUpgrades("push755.example.test") {
		t.Run(c.name, func(t *testing.T) {
			if gh755HostKey(t, c.want) == gh755HostKey(t, c.other) {
				t.Fatalf("fixture: %q and %q dial one key, so this case proves nothing", c.want, c.other)
			}
			id, priv, pub := env.connectSite(t, c.stored)
			env.prober.allowPing(c.want)
			env.prober.allowPing(c.other)
			before, beforeR := env.prober.calls(t)

			status, body := env.pushDiagnostics(t, priv, pub, c.reported)
			if status != http.StatusOK {
				t.Fatalf("diagnostics push answered %d, want 200: %v", status, body)
			}
			s := env.site(t, tenant, id)
			assertDialsSavedHost(t, s.URL, c.stored)
			if s.URL != c.want {
				t.Fatalf("site url = %q, want %q", s.URL, c.want)
			}
			changed, mismatch := env.urlAudits(t, tenant, id)
			assertPushURLChanged(t, changed, c.stored, c.want, "agent_diagnostics")
			if len(mismatch) != 0 {
				t.Fatalf("unexpected mismatch rows: %v", mismatch)
			}
			if pings := env.prober.pingsSince(before); len(pings) != 1 || pings[0] != c.want {
				t.Fatalf("pinged %v, want exactly [%s]", pings, c.want)
			}
			if _, afterR := env.prober.calls(t); afterR != beforeR {
				t.Fatalf("a scheme-only change asked for a redirect (%d->%d)", beforeR, afterR)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (l) the push never waits for the probe. With a site that takes 15s to
// answer a signed ping, both pushes answer 200 in well under a second, each
// with one job queued and no probe sent; the job, when run, performs the
// adoption; and a queue that refuses the job leaves both pushes answering
// 200.
// ---------------------------------------------------------------------------

// gh755SlowProber answers no probe until release is closed, the caller's
// context ends, or 15s pass, the way a slow site answers a signed ping.
type gh755SlowProber struct {
	release chan struct{}
	calls   atomic.Int32
}

func (p *gh755SlowProber) wait(ctx context.Context) {
	p.calls.Add(1)
	select {
	case <-p.release:
	case <-ctx.Done():
	case <-time.After(15 * time.Second):
	}
}

func (p *gh755SlowProber) CommandRedirectTarget(ctx context.Context, _ uuid.UUID, _ string) (string, bool, bool) {
	p.wait(ctx)
	return "", false, false
}

func (p *gh755SlowProber) CommandPingOK(ctx context.Context, _ uuid.UUID, _ string) (bool, bool) {
	p.wait(ctx)
	return false, false
}

func TestGH755Push_AdoptionNeverHoldsThePush(t *testing.T) {
	env, tenant := gh755PushSetup(t, "gh755p-async")
	ctx := context.Background()

	t.Run("a slow probe holds neither push", func(t *testing.T) {
		slow := &gh755SlowProber{release: make(chan struct{})}
		defer close(slow.release)
		env.svc.SetCommandRedirectProber(slow)
		defer env.svc.SetCommandRedirectProber(env.prober)
		id, priv, pub := env.connectSite(t, "http://pl1.example.com")

		pushes := []struct {
			name, path string
			body       []byte
		}{
			{"diagnostics", "/agent/v1/diagnostics", diagnosticsBody(t, "https://pl1.example.com")},
			{"metadata", "/agent/v1/metadata", metadataBody(t, "https://pl1.example.com", "0.61.150")},
		}
		for _, p := range pushes {
			start := time.Now()
			status, out := env.signedPush(t, priv, pub, p.path, p.body)
			took := time.Since(start)
			if status != http.StatusOK {
				t.Fatalf("%s push answered %d, want 200: %v", p.name, status, out)
			}
			if took >= time.Second {
				t.Fatalf("%s push took %v; it must not wait for the probe", p.name, took)
			}
		}
		if n := slow.calls.Load(); n != 0 {
			t.Fatalf("a push sent %d probes; only the job may probe", n)
		}
		jobs := env.adoptQ.take()
		want := []site.AdoptReportedURLArgs{
			{TenantID: tenant, SiteID: id, Reported: "https://pl1.example.com", Source: "agent_diagnostics"},
			{TenantID: tenant, SiteID: id, Reported: "https://pl1.example.com", Source: "agent_metadata", AgentVersion: "0.61.150"},
		}
		if !reflect.DeepEqual(jobs, want) {
			t.Fatalf("queued jobs = %+v, want %+v", jobs, want)
		}
		if s := env.site(t, tenant, id); s.URL != "http://pl1.example.com" {
			t.Fatalf("site url = %q before any job ran, want it unchanged", s.URL)
		}

		// The job, when run, performs the adoption.
		env.svc.SetCommandRedirectProber(env.prober)
		env.prober.allowPing("https://pl1.example.com")
		if err := env.adoptQ.EnqueueAdoptURL(ctx, jobs[0]); err != nil {
			t.Fatal(err)
		}
		env.runAdoptJobs(t)
		if s := env.site(t, tenant, id); s.URL != "https://pl1.example.com" {
			t.Fatalf("site url = %q after the job ran, want https://pl1.example.com", s.URL)
		}
		changed, mismatch := env.urlAudits(t, tenant, id)
		assertPushURLChanged(t, changed, "http://pl1.example.com", "https://pl1.example.com", "agent_diagnostics")
		if len(mismatch) != 0 {
			t.Fatalf("unexpected mismatch rows: %v", mismatch)
		}
	})

	t.Run("an enqueue failure leaves both pushes answering 200", func(t *testing.T) {
		env.adoptQ.mu.Lock()
		env.adoptQ.err = errors.New("queue unavailable")
		env.adoptQ.mu.Unlock()
		defer func() {
			env.adoptQ.mu.Lock()
			env.adoptQ.err = nil
			env.adoptQ.mu.Unlock()
		}()
		id, priv, pub := env.connectSite(t, "http://pl2.example.com")
		env.prober.allowPing("https://pl2.example.com")
		pings, _ := env.prober.calls(t)

		if status, out := env.pushDiagnostics(t, priv, pub, "https://pl2.example.com"); status != http.StatusOK {
			t.Fatalf("diagnostics push answered %d, want 200: %v", status, out)
		}
		if status, out := env.pushMetadata(t, priv, pub, "https://pl2.example.com"); status != http.StatusOK {
			t.Fatalf("metadata push answered %d, want 200: %v", status, out)
		}
		if s := env.site(t, tenant, id); s.URL != "http://pl2.example.com" {
			t.Fatalf("site url = %q, want it unchanged when the job was never queued", s.URL)
		}
		if after, _ := env.prober.calls(t); after != pings {
			t.Fatalf("pinged with no job queued (%d->%d)", pings, after)
		}
	})
}

// ---------------------------------------------------------------------------
// (m) the job on a real River client: a push inserts it, later pushes of the
// same address inside the hour insert nothing more, and the worker production
// registers adopts the address when River runs it.
// ---------------------------------------------------------------------------

func TestGH755Push_AdoptionJobRunsOnRiver(t *testing.T) {
	env, tenant := gh755PushSetup(t, "gh755p-river")
	ctx := context.Background()

	owner := connectOwner(t, env.pool)
	defer owner.Close()
	migrator, err := rivermigrate.New(riverpgxv5.New(owner.Pool), nil)
	if err != nil {
		t.Fatalf("river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate: %v", err)
	}

	// Insert only, so the jobs stay queued and can be counted.
	inserter, err := river.NewClient(riverpgxv5.New(env.pool.Pool), &river.Config{})
	if err != nil {
		t.Fatalf("river insert-only client: %v", err)
	}
	env.svc.SetAdoptURLEnqueuer(site.NewRiverAdoptURLEnqueuer(inserter))

	id, priv, pub := env.connectSite(t, "http://pr1.example.com")
	env.prober.allowPing("https://pr1.example.com")
	for i := 0; i < 2; i++ {
		if status, out := env.signedPush(t, priv, pub, "/agent/v1/diagnostics", diagnosticsBody(t, "https://pr1.example.com")); status != http.StatusOK {
			t.Fatalf("diagnostics push %d answered %d, want 200: %v", i, status, out)
		}
	}
	if status, out := env.signedPush(t, priv, pub, "/agent/v1/metadata", metadataBody(t, "https://pr1.example.com", "0.61.150")); status != http.StatusOK {
		t.Fatalf("metadata push answered %d, want 200: %v", status, out)
	}
	var queued int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'site_id' = $2`,
		site.AdoptReportedURLArgs{}.Kind(), id.String()).Scan(&queued); err != nil {
		t.Fatalf("count queued jobs: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued adoption jobs = %d after three pushes of one address, want 1", queued)
	}
	if s := env.site(t, tenant, id); s.URL != "http://pr1.example.com" {
		t.Fatalf("site url = %q before any worker ran, want it unchanged", s.URL)
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, site.NewAdoptReportedURLWorker(env.svc))
	client, err := river.NewClient(riverpgxv5.New(env.pool.Pool), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers: workers,
	})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("river start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()

	// Bounded: 80 polls of 250ms.
	got := ""
	for i := 0; i < 80; i++ {
		got = env.site(t, tenant, id).URL
		if got == "https://pr1.example.com" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if got != "https://pr1.example.com" {
		t.Fatalf("site url = %q after 20s of River running the job, want https://pr1.example.com", got)
	}
	changed, mismatch := env.urlAudits(t, tenant, id)
	assertPushURLChanged(t, changed, "http://pr1.example.com", "https://pr1.example.com", "agent_diagnostics")
	if len(mismatch) != 0 {
		t.Fatalf("unexpected mismatch rows: %v", mismatch)
	}
}
