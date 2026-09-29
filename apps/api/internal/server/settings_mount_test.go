package server

// settings_mount_test.go: the instance SMTP settings routes are mounted by
// New on the authenticated group that does not require an active
// organisation. Instance authority belongs to the person, so a principal that
// holds it and has no active organisation must reach the handler. This runs in
// CI with no database: the handlers New registers unconditionally are built
// over nil services (registration touches none of them), the session store is
// in memory, and the principal is placed on the request context, which
// Authenticate leaves in place when the request carries no credentials.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/settings"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
)

// mountGate answers both instance-authority reads with fixed values.
type mountGate struct{ superadmin bool }

func (g mountGate) IsSuperadmin(context.Context, uuid.UUID) (bool, error) {
	return g.superadmin, nil
}

func (g mountGate) SoleLiveTenantOwnedBy(context.Context, uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, nil
}

// SelfHosted is false (hosted), so the install-owner arm is never read.
func (g mountGate) SelfHosted() bool { return false }

func (g mountGate) InstallOwnerHomeTenant(context.Context, uuid.UUID) (bool, uuid.UUID, error) {
	return false, uuid.Nil, nil
}

var _ admingate.InstanceEmailStore = mountGate{}

func settingsMountEngine(t *testing.T, gate admingate.InstanceEmailStore) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	sessions := auth.NewSessionManagerWithStore(scs.New(), false)
	srv := New(Deps{
		Config:    config.Config{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sessions:  sessions,
		Auth:      middleware.NewAuthenticator(sessions, nil, nil, nil),
		AuthH:     auth.NewHandler(nil, sessions, nil, nil),
		MembersH:  auth.NewMembersHandler(nil, nil),
		APIKeyH:   apikey.NewHandler(nil, nil),
		AuditH:    audit.NewHandler(nil),
		TenantH:   tenant.NewHandler(nil, nil),
		SiteH:     site.NewHandler(nil, nil, ""),
		SettingsH: settings.NewHandler(nil, nil, gate),
	})
	e, ok := srv.Handler().(*gin.Engine)
	if !ok {
		t.Fatalf("Handler() is %T, want *gin.Engine", srv.Handler())
	}
	return e
}

// putSMTP sends PUT /api/v1/settings/smtp with a body that is not JSON, as p.
// A request the instance gate admits stops in the handler at 422
// invalid_body without reaching the (nil) service.
func putSMTP(e *gin.Engine, p domain.Principal) (int, string) {
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/smtp", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(domain.WithPrincipal(req.Context(), p))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body.Code
}

func noOrganisationUser() domain.Principal {
	return domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.Nil}
}

// Instance settings require instance-level authority, not an active
// organisation: a caller that holds the authority and has no active
// organisation reaches the handler.
func TestNew_InstanceSMTPSettings_ReachableWithoutActiveOrganisation(t *testing.T) {
	status, code := putSMTP(settingsMountEngine(t, mountGate{superadmin: true}), noOrganisationUser())
	if status != http.StatusUnprocessableEntity || code != "invalid_body" {
		t.Fatalf("PUT /api/v1/settings/smtp with instance authority and no active organisation: got %d %q; want 422 invalid_body from the handler", status, code)
	}
}

// Control: the same route with the same principal and no authority is refused
// by the instance gate, so the route is mounted and gated rather than absent.
func TestNew_InstanceSMTPSettings_GatedWithoutAuthority(t *testing.T) {
	status, code := putSMTP(settingsMountEngine(t, mountGate{}), noOrganisationUser())
	if status != http.StatusForbidden || code != settings.InstanceAuthorityRequiredCode {
		t.Fatalf("PUT /api/v1/settings/smtp without instance authority: got %d %q; want 403 %q", status, code, settings.InstanceAuthorityRequiredCode)
	}
}
