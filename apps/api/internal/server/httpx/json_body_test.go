package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestRequireJSONBody refuses every media type a cross-site page can send
// without a CORS preflight, and a missing or malformed header, with 415 and
// without running the handler; application/json, with parameters and in any
// case, passes through.
func TestRequireJSONBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		contentType string
		setHeader   bool
		wantPass    bool
	}{
		{"application/json", true, true},
		{"application/json; charset=utf-8", true, true},
		{"Application/JSON", true, true},
		{"text/plain", true, false},
		{"text/plain; charset=utf-8", true, false},
		{"application/x-www-form-urlencoded", true, false},
		{"multipart/form-data; boundary=x", true, false},
		{"application/json-seq", true, false},
		{"application/jsonx", true, false},
		{";;;", true, false},
		{"", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.contentType, func(t *testing.T) {
			ran := false
			r := gin.New()
			r.POST("/x", RequireJSONBody(), func(c *gin.Context) {
				ran = true
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"a":1}`))
			if tc.setHeader {
				req.Header.Set("Content-Type", tc.contentType)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if tc.wantPass {
				if !ran || w.Code != http.StatusNoContent {
					t.Fatalf("Content-Type %q: status %d, handler ran=%v; want it admitted",
						tc.contentType, w.Code, ran)
				}
				return
			}
			if ran {
				t.Fatalf("Content-Type %q reached the handler", tc.contentType)
			}
			if w.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("Content-Type %q: status %d, want 415", tc.contentType, w.Code)
			}
			if !strings.Contains(w.Body.String(), CodeUnsupportedMediaType) {
				t.Fatalf("Content-Type %q: body %s does not carry %q",
					tc.contentType, w.Body.String(), CodeUnsupportedMediaType)
			}
		})
	}
}
