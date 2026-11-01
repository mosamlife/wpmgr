package diagnostics

// GH #1005. The operator's "Re-run all checks" answers a site that refused or
// garbled the signed request with a 502 that names the cause, never a 500
// internal_error. These tests drive the production chain: Handler.refresh,
// Service.RefreshAgent, RefreshEnqueuerImpl, a real agentcmd.Client (real
// Signer, the SSRF-hardened httpclient with its test-only private-network
// switch) and an httptest site that plays the part of the WordPress site.
//
// The expected codes and statuses are literals on purpose: they are the
// contract the dashboard keys on, so a renamed constant must not be able to
// rename them silently.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// siteMarker stands for text the site supplied. A fake site puts it in every
// body it sends; it must never reach the operator.
const siteMarker = "site-supplied-QZXJ"

// commandPath is the route a signed diagnostics command is POSTed to.
const commandPath = "/wp-json/wpmgr/v1/command/diagnostics"

// testSiteLookup resolves every site to one address, or fails with err.
type testSiteLookup struct {
	url string
	err error
}

func (l testSiteLookup) GetSiteURL(context.Context, uuid.UUID, uuid.UUID) (string, error) {
	return l.url, l.err
}

// seenRequest is what a fake site recorded of one signed command.
type seenRequest struct {
	method, path, userAgent, authorization string
}

// fakeSite is an httptest site that records every request it receives and
// answers with behave.
type fakeSite struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []seenRequest
}

func newFakeSite(t *testing.T, behave http.HandlerFunc) *fakeSite {
	t.Helper()
	s := &fakeSite{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.got = append(s.got, seenRequest{r.Method, r.URL.Path, r.UserAgent(), r.Header.Get("Authorization")})
		s.mu.Unlock()
		behave(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fakeSite) requests() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenRequest(nil), s.got...)
}

// reply answers every request with a fixed status, content type and body.
func reply(status int, contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// stall holds the request open until the control plane gives up on it. The
// body is read first: the server only notices a closed connection, and cancels
// the request context, once it has been.
func stall(_ http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

// hostForbiddenPage is the 795-byte text/html 403 a web server or a host's
// firewall sends in front of WordPress: the page the reporter saw, not the
// agent's answer.
func hostForbiddenPage(t *testing.T) string {
	t.Helper()
	const size = 795
	head := "<!DOCTYPE html><html><head><title>403 Forbidden</title></head><body><h1>Forbidden</h1>" +
		"<p>You don't have permission to access this resource.</p>" +
		"<p>Additionally, a 403 Forbidden error was encountered while trying to use an ErrorDocument to handle the request.</p>" +
		"<p>Request id: " + siteMarker + "</p>"
	const tail = "</body></html>"
	page := head + strings.Repeat(" ", size-len(head)-len(tail)) + tail
	if len(page) != size {
		t.Fatalf("fixture is %d bytes, want %d", len(page), size)
	}
	return page
}

// refreshChain is the production wiring of the operator refresh with a real
// signed-command client in place of a fake.
type refreshChain struct {
	svc     *Service
	enq     *RefreshEnqueuerImpl
	handler *Handler
}

func newRefreshChain(t *testing.T, repo repository, sites SiteLookup, clientTimeout time.Duration) *refreshChain {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatal(err)
	}
	client := agentcmd.NewClient(httpclient.New(httpclient.Config{AllowPrivateNetworks: true, Timeout: clientTimeout}), signer)
	svc := NewService(repo)
	enq := NewRefreshEnqueuer(client, sites, svc)
	if enq == nil {
		t.Fatal("NewRefreshEnqueuer returned nil")
	}
	svc.SetRefreshEnqueuer(enq)
	// The audit recorder is nil: every case here ends before the handler
	// records the success, and a case that does not is caught below.
	return &refreshChain{svc: svc, enq: enq, handler: NewHandler(svc, nil)}
}

// apiAnswer is the response the control plane gave the dashboard.
type apiAnswer struct {
	siteID  string
	status  int
	code    string
	message string
	body    string
}

// refresh calls POST /sites/{id}/diagnostics/refresh through the handler.
func (c *refreshChain) refresh(t *testing.T) apiAnswer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	siteID := uuid.NewString()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Params = gin.Params{{Key: "siteId", Value: siteID}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sites/x/diagnostics/refresh", nil)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the refresh reached its success path (the audit record) where a failure answer was expected: %v", r)
			}
		}()
		c.handler.refresh(ctx)
	}()
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return apiAnswer{siteID: siteID, status: rec.Code, code: env.Code, message: env.Message, body: rec.Body.String()}
}

// TestRefreshAnswersASiteFailureWithBadGateway: whatever the site does to a
// signed diagnostics command, the operator gets a status and code that name
// what happened and wording the control plane wrote, never the site's body or
// free text. The redirect case shows the one sanitised address the copy may
// carry, by design.
func TestRefreshAnswersASiteFailureWithBadGateway(t *testing.T) {
	phpWarning := "\nWarning: Undefined variable $" + siteMarker + " in /var/www/html/wp-content/plugins/other/other.php on line 7\n"
	cases := []struct {
		name         string
		behave       http.HandlerFunc
		timeout      time.Duration
		siteURL      func(s *fakeSite) string // nil: the fake site's own address
		lookupErr    error
		repo         func() repository // nil: the in-memory ingest repository
		wantStatus   int
		wantCode     string
		wantContains string // lower-case fragment of the operator copy
		wantRequests int
	}{
		{
			name:         "host 403 page in front of WordPress",
			behave:       reply(403, "text/html", hostForbiddenPage(t)),
			wantStatus:   502,
			wantCode:     "site_refused",
			wantContains: "firewall or security rule blocked the request",
			wantRequests: 1,
		},
		{
			name:         "agent 403 token expired",
			behave:       reply(403, "application/json", `{"code":"wpmgr_token_expired","message":"Forbidden. `+siteMarker+`","data":{"status":403}}`),
			wantStatus:   502,
			wantCode:     "site_refused",
			wantContains: "clock",
			wantRequests: 1,
		},
		{
			name:         "site answers too slowly",
			behave:       stall,
			timeout:      150 * time.Millisecond,
			wantStatus:   502,
			wantCode:     "site_timeout",
			wantContains: "did not answer in time",
			wantRequests: 1,
		},
		{
			name:         "PHP warning printed before the JSON",
			behave:       reply(200, "application/json", phpWarning+string(pushBodyFixture(t))),
			wantStatus:   502,
			wantCode:     "site_bad_reply",
			wantContains: "was not the wpmgr agent's response",
			wantRequests: 1,
		},
		{
			name:         "reply that is JSON but not an object",
			behave:       reply(200, "application/json", `[1,2,3]`),
			wantStatus:   502,
			wantCode:     "site_bad_reply",
			wantContains: "was not the wpmgr agent's response",
			wantRequests: 1,
		},
		{
			name:         "empty 200 reply",
			behave:       reply(200, "application/json", ``),
			wantStatus:   502,
			wantCode:     "site_bad_reply",
			wantContains: "was not the wpmgr agent's response",
			wantRequests: 1,
		},
		{
			name:         "redirect to another address",
			behave:       redirectTo("https://redirect-target.example.test"+commandPath, "Moved "+siteMarker),
			wantStatus:   502,
			wantCode:     "site_url_redirects",
			wantContains: "exempt /wp-json/wpmgr/ from the redirect",
			wantRequests: 1,
		},
		{
			name: "agent reports its own failure",
			behave: reply(500, "application/json", `{"code":"wpmgr_command_failed","message":"Command execution failed: RuntimeException: boom `+siteMarker+`",`+
				`"data":{"status":500,"command":"diagnostics","exception":"RuntimeException","at":"includes/commands/class-diagnostics-command.php:77"}}`),
			wantStatus:   502,
			wantCode:     "agent_command_failed",
			wantContains: "stopped with an error",
			wantRequests: 1,
		},
		{
			name:         "web server error page",
			behave:       reply(500, "text/html", `<html><body>Internal Server Error `+siteMarker+`</body></html>`),
			wantStatus:   502,
			wantCode:     "site_error",
			wantContains: "server error (http 500)",
			wantRequests: 1,
		},
		{
			name:         "agent route not found",
			behave:       reply(404, "application/json", `{"code":"rest_no_route","message":"No route was found `+siteMarker+`","data":{"status":404}}`),
			wantStatus:   502,
			wantCode:     "site_error",
			wantContains: "did not find the wpmgr agent",
			wantRequests: 1,
		},
		{
			name:         "site refuses the connection",
			behave:       reply(200, "application/json", `{}`),
			siteURL:      closedSiteURL,
			wantStatus:   502,
			wantCode:     "site_unreachable",
			wantContains: "could not connect to the site",
			wantRequests: 0,
		},
		{
			name:         "reply without collected_at keeps its 422",
			behave:       reply(200, "application/json", string(pushBody(t, nil, true))),
			wantStatus:   422,
			wantCode:     "collected_at_missing",
			wantContains: "no collection time",
			wantRequests: 1,
		},
		{
			name:         "unknown site keeps its 404",
			behave:       reply(200, "application/json", `{}`),
			lookupErr:    domain.NotFound("site_not_found", "site not found"),
			wantStatus:   404,
			wantCode:     "site_not_found",
			wantContains: "site not found",
			wantRequests: 0,
		},
		{
			name:         "a failing database is not blamed on the site",
			behave:       reply(200, "application/json", string(pushBodyFixture(t))),
			repo:         func() repository { return failingUpsertRepo{&fakeRepo{}} },
			wantStatus:   500,
			wantCode:     "internal_error",
			wantContains: "internal server error",
			wantRequests: 1,
		},
		{
			name:         "an unusable saved address is not blamed on the site",
			behave:       reply(200, "application/json", `{}`),
			siteURL:      func(*fakeSite) string { return "ftp://example.test" },
			wantStatus:   500,
			wantCode:     "internal_error",
			wantContains: "internal server error",
			wantRequests: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			site := newFakeSite(t, tc.behave)
			url := site.srv.URL
			if tc.siteURL != nil {
				url = tc.siteURL(site)
			}
			var repo repository = &fakeRepo{}
			if tc.repo != nil {
				repo = tc.repo()
			}
			timeout := tc.timeout
			if timeout == 0 {
				timeout = 2 * time.Second
			}
			chain := newRefreshChain(t, repo, testSiteLookup{url: url, err: tc.lookupErr}, timeout)

			got := chain.refresh(t)

			if got.status != tc.wantStatus || got.code != tc.wantCode {
				t.Fatalf("answered %d %q (%q), want %d %q", got.status, got.code, got.message, tc.wantStatus, tc.wantCode)
			}
			if !strings.Contains(strings.ToLower(got.message), tc.wantContains) {
				t.Errorf("message %q does not contain %q", got.message, tc.wantContains)
			}
			for _, banned := range []string{"<html", "Forbidden.", siteMarker, "RuntimeException", "class-diagnostics-command", "/var/www"} {
				if strings.Contains(got.body, banned) {
					t.Errorf("the response carries site text %q: %s", banned, got.body)
				}
			}
			seen := site.requests()
			if len(seen) != tc.wantRequests {
				t.Fatalf("the site received %d requests, want %d", len(seen), tc.wantRequests)
			}
			for _, r := range seen {
				if r.method != http.MethodPost || r.path != commandPath {
					t.Errorf("the site received %s %s, want POST %s", r.method, r.path, commandPath)
				}
				if r.userAgent != "WPMgr-CommandClient/1.0" {
					t.Errorf("the site saw User-Agent %q, want WPMgr-CommandClient/1.0", r.userAgent)
				}
			}
		})
	}
}

// TestRefreshSucceedsAgainstAHealthySite is the positive control for the
// table above: the same chain, with a healthy site, stores the report and
// returns no error, so the failures above come from the site's replies and
// not from the harness.
func TestRefreshSucceedsAgainstAHealthySite(t *testing.T) {
	site := newFakeSite(t, reply(200, "application/json", string(pushBodyFixture(t))))
	repo := &fakeRepo{}
	chain := newRefreshChain(t, repo, testSiteLookup{url: site.srv.URL}, 2*time.Second)

	if err := chain.svc.RefreshAgent(context.Background(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("RefreshAgent: %v", err)
	}
	if len(repo.upserts) == 0 {
		t.Fatal("the report was not stored")
	}
	seen := site.requests()
	if len(seen) != 1 || seen[0].userAgent != "WPMgr-CommandClient/1.0" {
		t.Fatalf("the site saw %+v, want one POST with User-Agent WPMgr-CommandClient/1.0", seen)
	}
}

// TestRefreshWithoutACommanderAnswers501: when no signed-command client is
// wired, the route answers 501 diagnostics_refresh_unwired, the status the
// OpenAPI contract documents (it said 503 until GH #1005).
func TestRefreshWithoutACommanderAnswers501(t *testing.T) {
	svc := NewService(&fakeRepo{})
	chain := &refreshChain{svc: svc, handler: NewHandler(svc, nil)}

	got := chain.refresh(t)

	if got.status != 501 || got.code != "diagnostics_refresh_unwired" {
		t.Fatalf("answered %d %q, want 501 diagnostics_refresh_unwired", got.status, got.code)
	}
}

// TestRefreshRejectsASiteIDThatIsNotAUUID: the route answers 422
// invalid_site_id, as the OpenAPI contract documents, before it contacts any
// site.
func TestRefreshRejectsASiteIDThatIsNotAUUID(t *testing.T) {
	site := newFakeSite(t, reply(200, "application/json", `{}`))
	chain := newRefreshChain(t, &fakeRepo{}, testSiteLookup{url: site.srv.URL}, 2*time.Second)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Params = gin.Params{{Key: "siteId", Value: "not-a-uuid"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sites/not-a-uuid/diagnostics/refresh", nil)

	chain.handler.refresh(ctx)

	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if rec.Code != 422 || env.Code != "invalid_site_id" {
		t.Fatalf("answered %d %q, want 422 invalid_site_id", rec.Code, env.Code)
	}
	if n := len(site.requests()); n != 0 {
		t.Fatalf("the site received %d requests, want 0", n)
	}
}

// failingUpsertRepo is the ingest repository with a broken database:
// UpsertDiagnostic fails with an error that is not a domain error.
type failingUpsertRepo struct{ *fakeRepo }

func (failingUpsertRepo) UpsertDiagnostic(context.Context, uuid.UUID, uuid.UUID, Category, json.RawMessage, time.Time) (Diagnostic, error) {
	return Diagnostic{}, errors.New("connection to the database was lost")
}

// pushBodyFixture is a valid diagnostics report with a collection time.
func pushBodyFixture(t *testing.T) []byte {
	t.Helper()
	return pushBody(t, int64(1748505600), false)
}

// redirectTo answers every request with a 302 to location.
func redirectTo(location, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, body)
	}
}

// closedSiteURL returns the address of a site that has just stopped
// listening, so a connection to it is refused.
func closedSiteURL(s *fakeSite) string {
	url := s.srv.URL
	s.srv.Close()
	return url
}

// TestRefreshLogsOneWarnLinePerSiteFailure: a failure that is the site's doing
// is logged once, with the site, the command, the code and (when the site gave
// one) the HTTP status. The line never carries the command token, the
// Authorization header, the reply body or the error text. A failure that is not
// the site's doing logs nothing from the enqueuer.
func TestRefreshLogsOneWarnLinePerSiteFailure(t *testing.T) {
	cases := []struct {
		name       string
		behave     http.HandlerFunc
		timeout    time.Duration
		lookupErr  error
		repo       func() repository
		wantCode   string // "" means no line is expected
		wantStatus string // "" means the line has no http_status
	}{
		{name: "host 403 page", behave: reply(403, "text/html", hostForbiddenPage(t)), wantCode: "site_refused", wantStatus: "403"},
		{name: "agent 403", behave: reply(403, "application/json", `{"code":"wpmgr_token_skew","message":"`+siteMarker+`","data":{"status":403}}`), wantCode: "site_refused", wantStatus: "403"},
		{name: "web server error page", behave: reply(500, "text/html", `<html>`+siteMarker+`</html>`), wantCode: "site_error", wantStatus: "500"},
		{name: "redirect", behave: redirectTo("https://redirect-target.example.test"+commandPath, siteMarker), wantCode: "site_url_redirects", wantStatus: "302"},
		{name: "timeout", behave: stall, timeout: 150 * time.Millisecond, wantCode: "site_timeout"},
		{name: "PHP warning before the JSON", behave: reply(200, "application/json", "\nWarning: "+siteMarker+"\n"+string(pushBodyFixture(t))), wantCode: "site_bad_reply"},
		{name: "reply without collected_at", behave: reply(200, "application/json", string(pushBody(t, nil, true)))},
		{name: "unknown site", behave: reply(200, "application/json", `{}`), lookupErr: domain.NotFound("site_not_found", "site not found")},
		{name: "failing database", behave: reply(200, "application/json", string(pushBodyFixture(t))), repo: func() repository { return failingUpsertRepo{&fakeRepo{}} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			site := newFakeSite(t, tc.behave)
			var repo repository = &fakeRepo{}
			if tc.repo != nil {
				repo = tc.repo()
			}
			timeout := tc.timeout
			if timeout == 0 {
				timeout = 2 * time.Second
			}
			chain := newRefreshChain(t, repo, testSiteLookup{url: site.srv.URL, err: tc.lookupErr}, timeout)
			logs := &bytes.Buffer{}
			chain.enq.logger = slog.New(slog.NewTextHandler(logs, nil))

			got := chain.refresh(t)

			var lines []string
			if s := strings.TrimSpace(logs.String()); s != "" {
				lines = strings.Split(s, "\n")
			}
			if tc.wantCode == "" {
				if len(lines) != 0 {
					t.Fatalf("want no log line, got %q", lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("want exactly 1 log line, got %d: %q", len(lines), lines)
			}
			line := lines[0]
			for _, want := range []string{"level=WARN", "site_id=" + got.siteID, "command=diagnostics", "code=" + tc.wantCode} {
				if !strings.Contains(line, want) {
					t.Errorf("log line %q does not contain %q", line, want)
				}
			}
			if tc.wantStatus != "" {
				if !strings.Contains(line, "http_status="+tc.wantStatus) {
					t.Errorf("log line %q does not contain http_status=%s", line, tc.wantStatus)
				}
			} else if strings.Contains(line, "http_status=") {
				t.Errorf("log line %q carries an http_status the site never gave", line)
			}

			// The credential the site was sent must be real for the check below
			// to mean anything.
			seen := site.requests()
			if len(seen) != 1 || !strings.HasPrefix(seen[0].authorization, "Bearer ") {
				t.Fatalf("the site saw %+v, want one request with a Bearer token", seen)
			}
			token := strings.TrimPrefix(seen[0].authorization, "Bearer ")
			if len(token) < 40 {
				t.Fatalf("token %q is too short to be a signed command token", token)
			}
			for _, banned := range []string{token, "Bearer", "Authorization", siteMarker, "<html", "body=", "rejected by agent", "Warning:"} {
				if strings.Contains(line, banned) {
					t.Errorf("log line carries %q: %s", banned, line)
				}
			}
		})
	}
}

// sinkReturning is an IngestSink whose IngestDiagnostics fails with err.
type sinkReturning struct{ err error }

func (s sinkReturning) IngestDiagnostics(context.Context, uuid.UUID, uuid.UUID, []byte) (int, error) {
	return 0, s.err
}

func (sinkReturning) ResolveHostProvider(context.Context, uuid.UUID, uuid.UUID, string, []byte) {}

// TestRefreshKeepsTheKindOfADomainErrorThatWrapsADecodeError: only an ingest
// error that is not a domain error is taken for the site's. A domain error
// keeps its kind even when its cause is a decode error.
func TestRefreshKeepsTheKindOfADomainErrorThatWrapsADecodeError(t *testing.T) {
	refusal := domain.Validation("collected_at_invalid", "fixed copy").WithCause(&json.SyntaxError{})
	enq := NewRefreshEnqueuer(&fakeDiagnosticsAgent{body: []byte(`{}`)}, fakeSiteLookup{}, sinkReturning{err: refusal})

	err := enq.EnqueueRefreshDiagnostics(context.Background(), uuid.New(), uuid.New())

	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation || de.Code != "collected_at_invalid" {
		t.Fatalf("err = %v, want the 422 validation error unchanged", err)
	}
}
