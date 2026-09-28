package tests

// Login admission control (GH #718), through the real server.New router, a
// real auth.Service over Postgres connected as wpmgr_app (startPostgres), and
// the login gate in enforce mode.
//
// The unit tests in internal/auth run over a Service with no database, so none
// of them reaches a successful Service.Login. This is the proof that a correct
// password gives its admission charge back through the real handler.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/server"
	"github.com/mosamlife/wpmgr/apps/api/internal/settings"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
)

// loginAdmissionBalancer is the address the load balancer appends after the
// client, so a two-hop chain reads "client, balancer".
const loginAdmissionBalancer = "192.0.2.1"

func postLoginThroughBalancer(e http.Handler, client, email, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", client+", "+loginAdmissionBalancer)
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

// TestLoginAdmissionSuccessfulSignInsAreNeverRefused: from one source, many
// more correct-password sign-ins than the pair and source budgets allow, to
// one account and across two, are all answered 200. Then, as a control that the
// gate is live on this router, wrong passwords from one source are refused at
// exactly the pair budget.
func TestLoginAdmissionSuccessfulSignInsAreNeverRefused(t *testing.T) {
	pool := startPostgres(t)
	tenantID := seedTenant(t, pool, "login-admission")
	svc := buildTwoFAService(t, pool)
	owner := seedTwoFAUser(t, pool, tenantID, "login-admission-owner@example.com")
	colleague := seedTwoFAUser(t, pool, tenantID, "login-admission-colleague@example.com")

	sessions := auth.NewSessionManagerWithStore(scs.New(), false)
	authH := auth.NewHandler(svc, sessions, nil, nil)
	authH.SetProxyHops(2)
	gate, err := auth.NewLoginGate("0123456789abcdef0123456789abcdef-login-admission", auth.LoginModeEnforce, 0)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	gate.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	authH.SetLoginGate(gate)

	gin.SetMode(gin.TestMode)
	srv := server.New(server.Deps{
		Config:    config.Config{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pool:      pool,
		Sessions:  sessions,
		Auth:      middleware.NewAuthenticator(sessions, svc, nil, pool),
		AuthH:     authH,
		MembersH:  auth.NewMembersHandler(nil, nil),
		APIKeyH:   apikey.NewHandler(nil, nil),
		AuditH:    audit.NewHandler(nil),
		TenantH:   tenant.NewHandler(nil, nil),
		SiteH:     site.NewHandler(nil, nil, ""),
		SettingsH: settings.NewHandler(nil, nil, nil),
	})
	e := srv.Handler()

	// The source budget is 60 and the pair budget 10 in a 15-minute window.
	// 70 to one account passes both; 70 more across two accounts passes the
	// source budget a second time over.
	const office = "198.51.100.20"
	for i := 0; i < 70; i++ {
		if w := postLoginThroughBalancer(e, office, owner.email, owner.password); w.Code != http.StatusOK {
			t.Fatalf("successful sign-in %d to one account from one source: %d %s", i+1, w.Code, w.Body.String())
		}
	}
	for i := 0; i < 70; i++ {
		u := owner
		if i%2 == 1 {
			u = colleague
		}
		if w := postLoginThroughBalancer(e, office, u.email, u.password); w.Code != http.StatusOK {
			t.Fatalf("successful sign-in %d across accounts from one source: %d %s", i+1, w.Code, w.Body.String())
		}
	}
	if strings.Contains(logs.String(), "login admission: refused") {
		t.Fatalf("a successful sign-in was refused.\nlog:\n%s", logs.String())
	}

	// Control: the gate is live on this router. Wrong passwords from another
	// source are admitted up to the pair budget and refused after it.
	const guesser = "198.51.100.77"
	for i := 0; i < 10; i++ {
		if w := postLoginThroughBalancer(e, guesser, owner.email, fmt.Sprintf("wrong-%d", i)); w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: %d %s, want 401", i+1, w.Code, w.Body.String())
		}
	}
	w := postLoginThroughBalancer(e, guesser, owner.email, "wrong-again")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("control: the 11th wrong password was %d (Retry-After %q), want 429 with Retry-After; the gate is not enforcing on this router",
			w.Code, w.Header().Get("Retry-After"))
	}
	// And the owner, from their own source, is still let in.
	if w := postLoginThroughBalancer(e, office, owner.email, owner.password); w.Code != http.StatusOK {
		t.Fatalf("the owner from their own source after the guessing run: %d %s", w.Code, w.Body.String())
	}
}
