package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// csrfContentTypes are the media types a cross-site page can POST without a
// CORS preflight, plus no header at all.
var csrfContentTypes = []string{
	"text/plain",
	"application/x-www-form-urlencoded",
	"multipart/form-data; boundary=x",
	"",
}

func postWithType(r *gin.Engine, path, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestConsentAndMintRefuseANonJSONBody drives the two cookie-authenticated
// grant-creating routes through their real mounts: a form, text/plain,
// multipart or header-less POST answers 415 and the store is never touched,
// so no row changes. The same body sent as application/json is admitted and
// reaches the store, which is the control that makes the refusal mean the
// middleware and not an unrelated failure.
func TestConsentAndMintRefuseANonJSONBody(t *testing.T) {
	routes := []struct {
		name  string
		path  string
		body  func(t *testing.T) string
		build func(t *testing.T, store *fakeStore) *gin.Engine
	}{
		{
			name: "POST consent",
			path: "/api/v1/oauth/mcp/consent",
			body: consentBody,
			build: func(t *testing.T, store *fakeStore) *gin.Engine {
				return consentRouterAs(t, auditedService(store), orgPrincipalWithRole("owner"))
			},
		},
		{
			name: "POST connections",
			path: APIV1Prefix + connectionsGroupPath,
			body: func(*testing.T) string { return `{"name":"headless-ci","site_scope_mode":"all"}` },
			build: func(t *testing.T, store *fakeStore) *gin.Engine {
				return newConnectionsRouter(t, store, orgPrincipal(uuid.New()))
			},
		},
	}
	for _, rt := range routes {
		for _, ct := range csrfContentTypes {
			t.Run(rt.name+" as "+ct, func(t *testing.T) {
				store := approvalStore()
				w := postWithType(rt.build(t, store), rt.path, ct, rt.body(t))
				if w.Code != http.StatusUnsupportedMediaType {
					t.Fatalf("status %d, want 415; body %s", w.Code, w.Body.String())
				}
				if calls := store.callLog(); len(calls) != 0 {
					t.Fatalf("a %q POST reached the store: %v", ct, calls)
				}
			})
		}
		t.Run(rt.name+" as application/json", func(t *testing.T) {
			store := approvalStore()
			w := postWithType(rt.build(t, store), rt.path, "application/json", rt.body(t))
			if w.Code == http.StatusUnsupportedMediaType {
				t.Fatalf("an application/json POST was refused with 415: %s", w.Body.String())
			}
			// The control must REACH the store, or "no store call" on the
			// refused arm would hold for a reason other than the middleware.
			if len(store.callLog()) == 0 {
				t.Fatalf("the application/json control never reached the store "+
					"(status %d: %s), so the refused arm proves nothing", w.Code, w.Body.String())
			}
		})
	}
}
