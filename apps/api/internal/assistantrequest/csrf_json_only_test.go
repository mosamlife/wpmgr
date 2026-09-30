package assistantrequest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
)

// csrfContentTypes are the media types a cross-site page can POST without a
// CORS preflight, plus no header at all.
var csrfContentTypes = []string{
	"text/plain",
	"application/x-www-form-urlencoded",
	"multipart/form-data; boundary=x",
	"",
}

type noSender struct{}

func (noSender) SendAssistantPurge(context.Context, uuid.UUID, string, perf.CDNCiphertext, perf.AssistantPurge) (perf.AssistantPurgeResult, error) {
	return perf.AssistantPurgeResult{}, nil
}
func (noSender) PublishAssistantPurge(context.Context, uuid.UUID, uuid.UUID, string) {}

type noCacheStore struct{}

func (noCacheStore) GetCDNCredentialsCiphertextTx(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) (perf.CDNCiphertext, error) {
	return perf.CDNCiphertext{}, nil
}
func (noCacheStore) MarkCachePurgedTx(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, string) error {
	return nil
}

// csrfRouter mounts the real Handler.Register behind a signed-in owner. The
// service's repository has NO pool, so the first statement either route
// would run panics; the recovery records that the request reached the
// database, which is the only way a row could change.
func csrfRouter(t *testing.T, p domain.Principal) (*gin.Engine, *bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	reached := false
	svc := NewService(NewRepo(nil), nil, nil, nil, nil)
	svc.SetSender(noSender{}, noCacheStore{})
	svc.SetWriteToolsEnabled(true)
	r := gin.New()
	r.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) {
		reached = true
		c.AbortWithStatus(http.StatusTeapot)
	}))
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	NewHandler(svc).Register(r.Group("/api/v1"))
	return r, &reached
}

// TestApproveAndDeclineRefuseANonJSONBody: a form, text/plain, multipart or
// header-less POST to approve or decline answers 415 and never reaches the
// database, so no row changes. The same request sent as application/json
// does reach it, which is the control that makes the refusal mean the
// middleware and not an unrelated failure.
func TestApproveAndDeclineRefuseANonJSONBody(t *testing.T) {
	owner := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(),
		Scope: domain.ScopeOrg, Role: "owner"}
	site, req := uuid.New(), uuid.New()
	body := `{"presented_digest":"abc"}`
	for _, action := range []string{"approve", "decline"} {
		path := "/api/v1/sites/" + site.String() + "/ai/requests/" + req.String() + "/" + action
		for _, ct := range csrfContentTypes {
			t.Run(action+" as "+ct, func(t *testing.T) {
				r, reached := csrfRouter(t, owner)
				w := post(r, path, ct, body)
				if w.Code != http.StatusUnsupportedMediaType {
					t.Fatalf("status %d, want 415; body %s", w.Code, w.Body.String())
				}
				if *reached {
					t.Fatalf("a %q POST reached the database", ct)
				}
			})
		}
		t.Run(action+" as application/json", func(t *testing.T) {
			r, reached := csrfRouter(t, owner)
			w := post(r, path, "application/json", body)
			if w.Code == http.StatusUnsupportedMediaType {
				t.Fatalf("an application/json POST was refused with 415: %s", w.Body.String())
			}
			if !*reached {
				t.Fatalf("the application/json control never reached the database (status %d: %s), "+
					"so the refused arm proves nothing", w.Code, w.Body.String())
			}
		})
	}
}

func post(r *gin.Engine, path, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
