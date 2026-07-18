// cache_capability_fields_test.go — GH #243: the site-card "Page Cache" /
// "Object Cache" capability dots used to infer state from installed-plugin
// slugs that can never exist (both features ship as drop-ins, not plugins),
// leaving the dots permanently gray. This asserts toAPI() surfaces the real
// Site.PageCacheEnabled / Site.ObjectCacheEnabled booleans (populated by
// repo.Get/List's PK-keyed LEFT JOIN onto site_perf_config/
// site_object_cache_config) verbatim, and that the OpenAPI-generated struct
// carries the exact JSON field names the web client wires to (mirrors
// TestSiteGenStructHasUptimeJSONTags's pattern).
package site

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
)

func TestToAPICacheCapabilityFields(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name               string
		pageCacheEnabled   bool
		objectCacheEnabled bool
	}{
		// "No config row" is modeled the same as "config row with the feature
		// off" at the Site-model level: repo.Get/List's COALESCE(...,false)
		// collapses both cases to false before the model is ever built, so
		// there is nothing further for toAPI to distinguish — both are
		// asserted at the repo/integration layer (see
		// tests/site_cache_capability_integration_test.go).
		{"both off", false, false},
		{"page cache on, object cache off", true, false},
		{"page cache off, object cache on", false, true},
		{"both on", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Site{
				ID:                 uuid.New(),
				TenantID:           uuid.New(),
				URL:                "https://example.com",
				Name:               "Example",
				Status:             "active",
				HealthStatus:       "healthy",
				ConnectionState:    StateConnected,
				Tags:               []string{},
				CreatedAt:          now,
				UpdatedAt:          now,
				PageCacheEnabled:   tc.pageCacheEnabled,
				ObjectCacheEnabled: tc.objectCacheEnabled,
			}

			out := toAPI(s)

			if out.PageCacheEnabled != tc.pageCacheEnabled {
				t.Errorf("PageCacheEnabled = %v, want %v", out.PageCacheEnabled, tc.pageCacheEnabled)
			}
			if out.ObjectCacheEnabled != tc.objectCacheEnabled {
				t.Errorf("ObjectCacheEnabled = %v, want %v", out.ObjectCacheEnabled, tc.objectCacheEnabled)
			}
		})
	}
}

// TestSiteGenStructHasCacheCapabilityJSONTags verifies that the generated
// gen.Site struct carries the exact JSON field names the web client wires to,
// asserting the OpenAPI contract and both regenerated codegens are in sync.
func TestSiteGenStructHasCacheCapabilityJSONTags(t *testing.T) {
	wantTags := map[string]string{
		"PageCacheEnabled":   "page_cache_enabled",
		"ObjectCacheEnabled": "object_cache_enabled",
	}
	st := reflect.TypeOf(gen.Site{})
	for fieldName, wantJSON := range wantTags {
		f, ok := st.FieldByName(fieldName)
		if !ok {
			t.Errorf("gen.Site has no field %q — regen may be stale or field was renamed", fieldName)
			continue
		}
		gotJSON := f.Tag.Get("json")
		if gotJSON != wantJSON {
			t.Errorf("gen.Site.%s json tag = %q, want %q", fieldName, gotJSON, wantJSON)
		}
		// Required (non-optional) field: the type must be the bare bool, not
		// an OptBool — a bug here would silently make the dot omit from the
		// JSON response instead of rendering as an explicit false.
		if f.Type.Kind() != reflect.Bool {
			t.Errorf("gen.Site.%s type = %s, want bool (required field, not optional)", fieldName, f.Type)
		}
	}
}
