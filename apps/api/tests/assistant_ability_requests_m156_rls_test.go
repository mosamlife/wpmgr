// m156 proofs: assistant_ability_requests is invisible outside its tenant and
// its site scope, an approval is bound to the digest the approver saw and
// happens once, its facts cannot move, and its rows cannot be deleted.
//
// Built as assistant_cache_purge_requests_m151_rls_test.go is: every read and
// write goes through the production dispatch (RunTenantTx / InTenantTx), every
// transaction under test asserts from inside that it is wpmgr_app with
// neither SUPERUSER nor BYPASSRLS, the leak assertion comes first, and where a
// statement exists in db/query/assistant_ability_requests.sql the proof calls
// the generated sqlc method, so the statement proven is the statement shipped.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// aarParams is an honest pending page-create request for one site. input is
// the exact JSON text; input_sha256 is computed from its bytes, as R2 says.
func aarParams(tenant, site, grant uuid.UUID, seed string) sqlc.InsertAbilityRequestParams {
	input := `{"post_type":"page","title":"Spring sale ` + seed + `","content_html":"<p>Hi</p>"}`
	sum := sha256.Sum256([]byte(input))
	return sqlc.InsertAbilityRequestParams{
		TenantID:           tenant,
		SiteID:             site,
		ProposedByGrantID:  grant,
		EntryID:            uuid.New(),
		EntrySha256:        acprHex("entry-" + seed),
		AbilityName:        "wpmgr/page-create",
		OperatorPermission: "site.content.edit",
		InputJson:          input,
		InputSha256:        hex.EncodeToString(sum[:]),
		TargetPostID:       nil,
		PrecheckDigest:     acprHex("precheck-" + seed),
		PreviewDigest:      acprStr(acprHex("preview-" + seed)),
		BaseFingerprint:    "new:" + acprHex("fp-"+seed),
		SiteLabel:          "Shop",
		SiteHost:           "shop.example.com",
		GrantLabel:         "Laptop",
		GrantVia:           "token",
		SetupClient:        acprStr("claude-code"),
		TitleExcerpt:       acprStr("Spring sale " + seed),
		Editor:             acprStr("wordpress_blocks"),
		PostType:           acprStr("page"),
		EffectCopy:         "draft",
		Snapshot:           "created_post_trash",
		CardCopyVersion:    1,
		DigestNonce:        acprHex("nonce-" + seed),
		PresentedDigest:    acprHex("digest-" + seed),
		ExpiresAt:          time.Now().UTC().Add(24 * time.Hour),
	}
}

func aarInsert(t *testing.T, pool *db.Pool, p domain.Principal, arg sqlc.InsertAbilityRequestParams) sqlc.AssistantAbilityRequest {
	t.Helper()
	var row sqlc.AssistantAbilityRequest
	if err := pool.RunTenantTx(context.Background(), p, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m156 insert)")
		var err error
		row, err = sqlc.New(tx).InsertAbilityRequest(context.Background(), arg)
		return err
	}); err != nil {
		t.Fatalf("insert ability request for site %s: %v", arg.SiteID, err)
	}
	return row
}

func aarCountVisible(t *testing.T, tx pgx.Tx, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(context.Background(),
		`SELECT count(*) FROM assistant_ability_requests WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count visible ability requests: %v", err)
	}
	return n
}

// aarApprove runs the shipped approve statement under the deciding person's
// own org-wide principal, as a person's approval is made, and returns its
// error unchanged, so a caller can assert the refusal.
func aarApprove(t *testing.T, pool *db.Pool, row sqlc.AssistantAbilityRequest, digest string) (sqlc.AssistantAbilityRequest, error) {
	t.Helper()
	var out sqlc.AssistantAbilityRequest
	decider := uuid.New()
	err := pool.RunTenantTx(context.Background(), acprDeciderPrincipal(row.TenantID, decider), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m156 approve)")
		var err error
		out, err = sqlc.New(tx).ApproveAbilityRequest(context.Background(), sqlc.ApproveAbilityRequestParams{
			DecidedByUserID:       decider,
			DispatchWindowSeconds: 3600,
			TenantID:              row.TenantID,
			ID:                    row.ID,
			SiteID:                row.SiteID,
			PresentedDigest:       digest,
		})
		return err
	})
	return out, err
}

// TestAbilityRequestIsolationAsAppRole proves the permissive tenant policy
// and both directions of the RESTRICTIVE site-scope policy.
//
// Mutation, planted and watched: DROP POLICY
// assistant_ability_requests_site_scope fires "SITE-SCOPE LEAK".
func TestAbilityRequestIsolationAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenantA := seedTenant(t, pool, "m156-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "m156-b-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenantA, "")
	s2 := seedSite(t, pool, tenantA, "")
	grant := uuid.New()

	r1 := aarInsert(t, pool, acprSitePrincipal(tenantA, s1, s2), aarParams(tenantA, s1, grant, "iso-1"))
	r2 := aarInsert(t, pool, acprSitePrincipal(tenantA, s1, s2), aarParams(tenantA, s2, grant, "iso-2"))

	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (foreign tenant)")
		if n := aarCountVisible(t, tx, r1.ID); n != 0 {
			t.Fatalf("TENANCY LEAK: tenant B sees tenant A's ability request %s (%d rows)", r1.ID, n)
		}
		var total int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM assistant_ability_requests`).Scan(&total); err != nil {
			return err
		}
		if total != 0 {
			t.Fatalf("tenant B sees %d ability requests in a table where it has written none", total)
		}
		return nil
	}); err != nil {
		t.Fatalf("foreign tenant read: %v", err)
	}

	if err := pool.RunTenantTx(ctx, acprSitePrincipal(tenantA, s1), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (single site S1)")
		if n := aarCountVisible(t, tx, r2.ID); n != 0 {
			t.Fatalf("SITE-SCOPE LEAK: a principal scoped to site %s sees ability request %s on site %s (%d rows); "+
				"assistant_ability_requests_site_scope is missing or not RESTRICTIVE", s1, r2.ID, s2, n)
		}
		if n := aarCountVisible(t, tx, r1.ID); n != 1 {
			t.Fatalf("OVER-FIRING: a principal scoped to site %s cannot see its own site's request %s (%d rows)", s1, r1.ID, n)
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		_, err = sqlc.New(sp).InsertAbilityRequest(ctx, aarParams(tenantA, s2, uuid.New(), "iso-smuggle"))
		_ = sp.Rollback(ctx)
		if err == nil {
			t.Fatalf("SITE-SCOPE WRITE LEAK: a principal scoped to site %s inserted an ability request for site %s", s1, s2)
		}
		if !strings.Contains(strings.ToLower(err.Error()), "row-level security") {
			t.Fatalf("the out-of-scope INSERT was refused, but not by row security: %v", err)
		}
		t.Logf("out-of-scope insert refused by row security: %v", err)
		return nil
	}); err != nil {
		t.Fatalf("single-site read and write: %v", err)
	}
}

// TestAbilityRequestApproveIsDigestBoundAndOnceAsAppRole proves an approval
// with a stale digest is refused and leaves the row waiting, the honest
// approval lands with a database-computed deadline, and a second approval
// (or a decline after it) is refused.
func TestAbilityRequestApproveIsDigestBoundAndOnceAsAppRole(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m156-ap-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	row := aarInsert(t, pool, acprSitePrincipal(tenant, site), aarParams(tenant, site, uuid.New(), "approve"))

	if _, err := aarApprove(t, pool, row, acprHex("a card the approver never saw")); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("STALE DIGEST APPROVED: approve with a digest that is not the stored one returned %v, want pgx.ErrNoRows", err)
	}
	t.Logf("stale-digest approve refused (no row)")

	got, err := aarApprove(t, pool, row, row.PresentedDigest)
	if err != nil {
		t.Fatalf("honest approve: %v", err)
	}
	if got.State != "approved" || !got.DecidedAt.Valid || !got.DispatchDeadlineAt.Valid || !got.DecidedByUserID.Valid {
		t.Fatalf("honest approve wrote state=%q decided_at=%v deadline=%v decider=%v",
			got.State, got.DecidedAt, got.DispatchDeadlineAt, got.DecidedByUserID)
	}
	if !got.DispatchDeadlineAt.Time.After(got.DecidedAt.Time) {
		t.Fatalf("dispatch deadline %v is not after the decision %v", got.DispatchDeadlineAt.Time, got.DecidedAt.Time)
	}

	if _, err := aarApprove(t, pool, row, row.PresentedDigest); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("SECOND APPROVE ACCEPTED: approving an approved row returned %v, want pgx.ErrNoRows", err)
	}
	t.Logf("second approve refused (no row)")

	if err := pool.RunTenantTx(context.Background(), acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m156 decline after approve)")
		_, err := sqlc.New(tx).DeclineAbilityRequest(context.Background(), sqlc.DeclineAbilityRequestParams{
			DecidedByUserID: uuid.New(), TenantID: tenant, ID: row.ID, SiteID: site,
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("DECLINE AFTER APPROVE ACCEPTED: returned %v, want pgx.ErrNoRows", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("decline after approve: %v", err)
	}
}

// TestAbilityRequestFactsImmutableAndRowUndeletableAsAppRole proves the
// column-privilege immutability and the DELETE/TRUNCATE revoke, with a
// positive control: a workflow column IS updatable in the same transaction,
// so the refusals are the column privileges and not a broken connection.
//
// Mutation, planted and watched: removing the m156 lines from the harness
// re-revoke list in rls_integration_test.go makes the refusals succeed.
func TestAbilityRequestFactsImmutableAndRowUndeletableAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m156-im-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	row := aarInsert(t, pool, acprSitePrincipal(tenant, site), aarParams(tenant, site, uuid.New(), "immutable"))

	immutable := []string{
		"id", "tenant_id", "site_id", "proposed_by_grant_id", "entry_id", "entry_sha256",
		"ability_name", "operator_permission", "input_json", "input_sha256", "target_post_id",
		"precheck_digest", "preview_digest", "base_fingerprint", "site_label", "site_host",
		"grant_label", "grant_via", "setup_client", "title_excerpt", "editor", "post_type",
		"effect_copy", "snapshot", "card_copy_version", "digest_nonce", "presented_digest",
		"created_at", "expires_at",
	}

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m156 privileges)")
		var pgErr *pgconn.PgError
		for _, col := range immutable {
			err := m133ExpectRefused(t, tx,
				`UPDATE assistant_ability_requests SET `+col+` = `+col+` WHERE id = $1`, row.ID)
			if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Fatalf("IMMUTABLE COLUMN WRITABLE: UPDATE of %s: want 42501, got %v", col, err)
			}
		}
		t.Logf("UPDATE refused 42501 on all %d immutable columns", len(immutable))

		err := m133ExpectRefused(t, tx, `DELETE FROM assistant_ability_requests WHERE id = $1`, row.ID)
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("ROW DELETABLE: DELETE: want 42501, got %v", err)
		}
		err = m133ExpectRefused(t, tx, `TRUNCATE assistant_ability_requests`)
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("TABLE TRUNCATABLE: TRUNCATE: want 42501, got %v", err)
		}
		t.Logf("DELETE and TRUNCATE refused 42501")

		// Positive control: a workflow column moves.
		tag, err := tx.Exec(ctx,
			`UPDATE assistant_ability_requests SET dispatch_attempts = dispatch_attempts WHERE id = $1`, row.ID)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("POSITIVE CONTROL FAILED: a workflow-column UPDATE returned %v, %d rows; "+
				"the refusals above prove nothing", err, tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("privileges: %v", err)
	}
}

// TestAbilityRequestInputHashAndCreationDedupeAsAppRole proves the input
// hash is held to the input bytes, an identical creation from the same
// connection dedupes to the waiting row, and a different page does not.
func TestAbilityRequestInputHashAndCreationDedupeAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m156-dd-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	grant := uuid.New()
	first := aarInsert(t, pool, acprSitePrincipal(tenant, site), aarParams(tenant, site, grant, "dedupe"))
	if first.TargetKey == nil || *first.TargetKey != "new:"+first.InputSha256 {
		t.Fatalf("target_key for a creation = %v, want new:%s", first.TargetKey, first.InputSha256)
	}

	if err := pool.RunTenantTx(ctx, acprSitePrincipal(tenant, site), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m156 dedupe)")
		q := sqlc.New(tx)

		// The same creation again: ON CONFLICT, no row.
		if _, err := q.InsertAbilityRequest(ctx, aarParams(tenant, site, grant, "dedupe")); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("DUPLICATE CREATION: an identical waiting creation was inserted again (err %v)", err)
		}
		again, err := q.GetPendingAbilityRequestForTarget(ctx, sqlc.GetPendingAbilityRequestForTargetParams{
			TenantID: tenant, SiteID: site, ProposedByGrantID: grant,
			AbilityName: "wpmgr/page-create", TargetKey: acprStr(*first.TargetKey),
		})
		if err != nil || again.ID != first.ID {
			t.Fatalf("dedupe read returned %v (err %v), want %s", again.ID, err, first.ID)
		}

		// A different page from the same connection is its own request.
		if _, err := q.InsertAbilityRequest(ctx, aarParams(tenant, site, grant, "another page")); err != nil {
			t.Fatalf("OVER-FIRING: a different creation was refused: %v", err)
		}

		// A hash that does not match the bytes is refused by the CHECK.
		bad := aarParams(tenant, site, uuid.New(), "bad-hash")
		bad.InputSha256 = acprHex("not the input")
		var pgErr *pgconn.PgError
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		_, err = sqlc.New(sp).InsertAbilityRequest(ctx, bad)
		_ = sp.Rollback(ctx)
		if err == nil || !errors.As(err, &pgErr) || pgErr.ConstraintName != "assistant_ability_requests_input_sha256_matches_check" {
			t.Fatalf("INPUT HASH UNBOUND: an input_sha256 that is not sha256(input_json) gave %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("dedupe: %v", err)
	}
}
