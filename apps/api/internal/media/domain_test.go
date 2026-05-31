package media

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestObjectKeys_TenantSiteJobNamespaced(t *testing.T) {
	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	siteID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	job := "JOB123"

	src := SrcKey(tenantID, siteID, job, "full")
	out := OutKey(tenantID, siteID, job, "full")

	wantPrefix := "media/" + tenantID.String() + "/" + siteID.String() + "/" + job
	if !strings.HasPrefix(src, wantPrefix+"/src/") {
		t.Errorf("src key %q lacks tenant/site/job/src prefix", src)
	}
	if !strings.HasPrefix(out, wantPrefix+"/out/") {
		t.Errorf("out key %q lacks tenant/site/job/out prefix", out)
	}
	// A presign can never target another tenant's prefix.
	other := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	if strings.Contains(SrcKey(other, siteID, job, "full"), tenantID.String()) {
		t.Error("cross-tenant key leakage")
	}
}

func TestValidTargetFormat(t *testing.T) {
	for _, f := range []string{"avif", "webp", "original"} {
		if !ValidTargetFormat(f) {
			t.Errorf("%q should be valid", f)
		}
	}
	for _, f := range []string{"", "gif", "png", "jpeg"} {
		if ValidTargetFormat(f) {
			t.Errorf("%q should be invalid", f)
		}
	}
}

func TestValidTargetQuality(t *testing.T) {
	for _, q := range []string{"", "lossy", "lossless"} {
		if !ValidTargetQuality(q) {
			t.Errorf("%q should be valid", q)
		}
	}
	if ValidTargetQuality("ultra") {
		t.Error("ultra should be invalid")
	}
}
