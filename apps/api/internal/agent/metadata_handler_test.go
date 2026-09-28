package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
)

// fakeMetadataSink is a MetadataSink double that records the decoded Metadata
// it was handed, so a test can assert what actually reached the sink rather
// than just the HTTP status.
type fakeMetadataSink struct {
	got       Metadata
	callCount int
}

func (f *fakeMetadataSink) ApplyAgentMetadata(_ context.Context, _, _ uuid.UUID, m Metadata) (gen.Site, error) {
	f.got = m
	f.callCount++
	return gen.Site{}, nil
}

func (f *fakeMetadataSink) Heartbeat(_ context.Context, _, _ uuid.UUID) error { return nil }

// postMetadata drives POST /agent/v1/metadata through a real gin engine (so
// gin flushes status/body to the recorder exactly as in production), with a
// middleware injecting the verified identity the agent auth would normally
// attach — the same pattern callManifest uses in update_handler_test.go.
func postMetadata(t *testing.T, body string) (*httptest.ResponseRecorder, *fakeMetadataSink) {
	t.Helper()
	sink := &fakeMetadataSink{}
	h := NewHandler(sink)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/agent/v1")
	g.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(WithIdentity(c.Request.Context(), Identity{SiteID: uuid.New(), TenantID: uuid.New()}))
		c.Next()
	})
	h.Register(g)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agent/v1/metadata", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w, sink
}

// TestMetadataHandlerToleratesMalformedKeystoreShapes (GH #753) proves that a
// malformed "keystore" value never fails the metadata push through the real
// handler: each shape below made the pre-fix plain-struct decode of
// keystoreStatusDTO return a JSON error, which 422'd the WHOLE push and lost
// plugins/themes/age_recipient with it. Every case here must get a 2xx and
// the sink must still receive the plugins/themes the same request carried.
func TestMetadataHandlerToleratesMalformedKeystoreShapes(t *testing.T) {
	const rest = `"plugins":[{"slug":"akismet/akismet.php","name":"Akismet","version":"5.3.1","active":true}],` +
		`"themes":[{"slug":"twentytwentyfour","name":"Twenty Twenty-Four","version":"1.0","active":true}]`

	cases := map[string]string{
		// PHP's json_encode(array()) renders an empty array as `[]`, not
		// `{}` — "items" is documented as an object.
		"items as PHP's empty array": `{"wp_version":"6.8",` + rest + `,"keystore":{"state":"ok","items":[]}}`,
		// The whole "keystore" value sent as something other than an object.
		"keystore as a string":       `{"wp_version":"6.8",` + rest + `,"keystore":"x"}`,
		"keystore as an empty array": `{"wp_version":"6.8",` + rest + `,"keystore":[]}`,
		"keystore as a number":       `{"wp_version":"6.8",` + rest + `,"keystore":5}`,
		// "unreadable" is documented as an array.
		"unreadable as an empty object": `{"wp_version":"6.8",` + rest + `,"keystore":{"state":"ok","unreadable":{}}}`,
		"unreadable as a string":        `{"wp_version":"6.8",` + rest + `,"keystore":{"state":"ok","unreadable":"x"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w, sink := postMetadata(t, body)
			if w.Code < 200 || w.Code >= 300 {
				t.Fatalf("status = %d, want 2xx; body: %s", w.Code, w.Body.String())
			}
			if sink.callCount != 1 {
				t.Fatalf("sink.ApplyAgentMetadata call count = %d, want 1", sink.callCount)
			}
			if len(sink.got.Plugins) != 1 || sink.got.Plugins[0].Slug != "akismet/akismet.php" {
				t.Fatalf("plugins lost to the malformed keystore shape: %+v", sink.got.Plugins)
			}
			if len(sink.got.Themes) != 1 || sink.got.Themes[0].Slug != "twentytwentyfour" {
				t.Fatalf("themes lost to the malformed keystore shape: %+v", sink.got.Themes)
			}
		})
	}
}

// TestMetadataHandlerStillDecodesWellFormedKeystore proves the tolerant
// decode above doesn't quietly swallow a well-formed probe: the honest case
// the guard above must not block.
func TestMetadataHandlerStillDecodesWellFormedKeystore(t *testing.T) {
	body := `{"wp_version":"6.8","plugins":[],"themes":[],` +
		`"keystore":{"state":"unreadable","key_source":"salts","items":{"age_identity":"unreadable"},"unreadable":["age_identity"]}}`
	w, sink := postMetadata(t, body)
	if w.Code < 200 || w.Code >= 300 {
		t.Fatalf("status = %d, want 2xx; body: %s", w.Code, w.Body.String())
	}
	if sink.got.KeystoreStatus == nil {
		t.Fatal("a well-formed keystore probe must still decode into a non-nil KeystoreStatus")
	}
	ks := sink.got.KeystoreStatus
	if ks.State != "unreadable" || ks.KeySource != "salts" {
		t.Fatalf("state/key_source not decoded: %+v", ks)
	}
	if len(ks.Items) != 1 || ks.Items["age_identity"] != "unreadable" {
		t.Fatalf("items not decoded: %+v", ks.Items)
	}
	if len(ks.Unreadable) != 1 || ks.Unreadable[0] != "age_identity" {
		t.Fatalf("unreadable not decoded: %+v", ks.Unreadable)
	}
}
