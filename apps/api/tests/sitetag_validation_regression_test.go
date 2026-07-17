// sitetag_validation_regression_test.go — GH #230 "rich tags" (m100)
// adversarial-verify follow-up (HIGH #2): two live write paths
// (MintEnrollmentInput / site-first "Add site", and the legacy POST /enroll's
// EnrollRequest) never capped tag length before this fix, letting an
// over-length tag reach sites.tags and later abort the m100 backfill (the
// CRITICAL this fix set accompanies). Proves both paths now reject an
// over-length tag with a validation error BEFORE any write — critically, for
// the site-first flow, before CreatePending's own committed transaction, so
// a rejected mint can never strand an orphaned pending_enrollment site.
// Requires Docker; skips when unavailable (via startPostgres).
package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// over64CharTag is exactly 65 characters — one past site_tags' char_length
// <= 64 CHECK (and the sitetag package's maxTagNameLen).
var over64CharTag = strings.Repeat("a", 65)

func TestMintEnrollmentCode_OverLengthTag_RejectedBeforeAnyWrite(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()
	tenant := seedTenant(t, pool, "mint-longtag-"+uuid.NewString()[:8])

	repo := site.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	conn := site.NewConnectionService(repo, domain.NewValidator(), rec, nil, domain.SystemClock{}, nil)

	const url = "https://mint-longtag.example.com"
	_, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{
		TenantID: tenant,
		URL:      url,
		Name:     "Long Tag Site",
		Tags:     []string{over64CharTag},
	})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Fatalf("MintEnrollmentCode with a 65-char tag: got %v, want a KindValidation domain error", err)
	}

	// CRITICAL ordering assertion: no orphaned pending_enrollment site was
	// created (CreatePending, step 1, must never have committed).
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM sites WHERE tenant_id = $1 AND url = $2`, tenant, url).Scan(&n); err != nil {
		t.Fatalf("count sites: %v", err)
	}
	if n != 0 {
		t.Fatalf("found %d site row(s) for a rejected mint, want 0 (no orphaned pending_enrollment site)", n)
	}
}

func TestMintEnrollmentCode_ValidTags_StillSucceeds(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "mint-validtag-"+uuid.NewString()[:8])

	repo := site.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	conn := site.NewConnectionService(repo, domain.NewValidator(), rec, nil, domain.SystemClock{}, nil)

	code, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{
		TenantID: tenant,
		URL:      "https://mint-validtag.example.com",
		Name:     "Valid Tag Site",
		Tags:     []string{"prod", "eu"},
	})
	if err != nil {
		t.Fatalf("MintEnrollmentCode with valid tags: %v", err)
	}
	if code.SiteID == uuid.Nil {
		t.Fatal("expected a minted site id")
	}
}

func TestEnroll_OverLengthTag_RejectedBeforeCodeConsumed(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "enroll-longtag-"+uuid.NewString()[:8])

	svc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	created, err := svc.CreatePairingCode(ctx, site.CreatePairingCodeInput{TenantID: tenant})
	if err != nil {
		t.Fatalf("create pairing code: %v", err)
	}

	_, _, pubB64 := genKey(t)
	_, err = svc.Enroll(ctx, site.EnrollRequest{
		PairingCode:    created.Plaintext,
		SiteURL:        "https://enroll-longtag.example.com",
		AgentPublicKey: pubB64,
		Tags:           []string{over64CharTag},
	})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Fatalf("Enroll with a 65-char tag: got %v, want a KindValidation domain error", err)
	}

	// The code must remain unconsumed — validation is the FIRST statement in
	// Enroll, before the pairing code is even looked up.
	s, err := svc.Enroll(ctx, site.EnrollRequest{
		PairingCode:    created.Plaintext,
		SiteURL:        "https://enroll-longtag-retry.example.com",
		AgentPublicKey: pubB64,
	})
	if err != nil {
		t.Fatalf("retry enroll with the same (still-valid) code: %v", err)
	}
	if s.TenantID != tenant {
		t.Fatalf("tenant = %v, want %v", s.TenantID, tenant)
	}
}

// TestEnroll_ManyTagsCap proves the max=50 tag-count cap (mirrors
// CreatePairingCodeInput's own cap) also rejects, for completeness.
func TestEnroll_ManyTagsCap(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "enroll-manytags-"+uuid.NewString()[:8])

	svc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	created, err := svc.CreatePairingCode(ctx, site.CreatePairingCodeInput{TenantID: tenant})
	if err != nil {
		t.Fatalf("create pairing code: %v", err)
	}

	tooMany := make([]string, 51)
	for i := range tooMany {
		tooMany[i] = "t" + strings.Repeat("x", 3) + string(rune('a'+i%26))
	}
	_, _, pubB64 := genKey(t)
	_, err = svc.Enroll(ctx, site.EnrollRequest{
		PairingCode:    created.Plaintext,
		SiteURL:        "https://enroll-manytags.example.com",
		AgentPublicKey: pubB64,
		Tags:           tooMany,
	})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Fatalf("Enroll with 51 tags: got %v, want a KindValidation domain error", err)
	}
}
