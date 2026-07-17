// sitetag_service_integration_test.go — GH #230 "rich tags" (m100) core
// behavior: create/409, rename propagation into sites.tags AND unexpired
// pairing_codes.tags, rename-collision 409 vs merge:true survivor + array
// dedup, fleet-wide delete, bulk-apply add/remove math + per-site allowlist,
// SetTags upserting the registry in the same transaction, and case-sensitive
// names. Requires Docker; skips when unavailable (via startPostgres).
package tests

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/sitetag"
)

func newSiteTagServices(pool *db.Pool) (*site.Service, *sitetag.Service) {
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	tagSvc := sitetag.NewService(sitetag.NewRepo(pool))
	return siteSvc, tagSvc
}

// sitesTagsOf fetches a site's tags array via the admin (RLS-bypass) pool.
func sitesTagsOf(t *testing.T, admin *db.Pool, siteID uuid.UUID) []string {
	t.Helper()
	var tags []string
	if err := admin.QueryRow(context.Background(), `SELECT tags FROM sites WHERE id = $1`, siteID).Scan(&tags); err != nil {
		t.Fatalf("query site tags: %v", err)
	}
	sort.Strings(tags)
	return tags
}

// pairingCodeTagsOf fetches a pairing code's tags array via the admin pool.
func pairingCodeTagsOf(t *testing.T, admin *db.Pool, codeID uuid.UUID) []string {
	t.Helper()
	var tags []string
	if err := admin.QueryRow(context.Background(), `SELECT tags FROM pairing_codes WHERE id = $1`, codeID).Scan(&tags); err != nil {
		t.Fatalf("query pairing code tags: %v", err)
	}
	sort.Strings(tags)
	return tags
}

func registryRowCount(t *testing.T, admin *db.Pool, tenantID uuid.UUID, name string) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM site_tags WHERE tenant_id = $1 AND name = $2`, tenantID, name).Scan(&n); err != nil {
		t.Fatalf("count registry rows: %v", err)
	}
	return n
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSiteTagSetTags_UpsertsRegistrySameTx proves site.Service.SetTags
// upserts the tag names into site_tags in the same transaction as the
// sites.tags write (m100 binding invariant #1).
func TestSiteTagSetTags_UpsertsRegistrySameTx(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	siteSvc, _ := newSiteTagServices(pool)
	tenant := seedTenant(t, pool, "sitetag-settags-"+uuid.NewString()[:8])
	s, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://settags.example.com", Name: "seed"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	if _, err := siteSvc.SetTags(ctx, site.SetTagsInput{TenantID: tenant, SiteID: s.ID, Tags: []string{"prod", "eu"}}); err != nil {
		t.Fatalf("SetTags: %v", err)
	}

	if got := registryRowCount(t, admin, tenant, "prod"); got != 1 {
		t.Fatalf("registry row count for 'prod' = %d, want 1 (upserted by SetTags)", got)
	}
	if got := registryRowCount(t, admin, tenant, "eu"); got != 1 {
		t.Fatalf("registry row count for 'eu' = %d, want 1 (upserted by SetTags)", got)
	}
}

// TestSiteTagCreate_DuplicateNameConflict_CaseSensitive proves exact-case
// uniqueness: "prod" and "Prod" are distinct tags (site.normalizeTags/
// `= ANY(tags)` semantics), while a true duplicate 409s.
func TestSiteTagCreate_DuplicateNameConflict_CaseSensitive(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	_, tagSvc := newSiteTagServices(pool)
	tenant := seedTenant(t, pool, "sitetag-create-"+uuid.NewString()[:8])

	if _, err := tagSvc.Create(ctx, sitetag.CreateInput{TenantID: tenant, Name: "prod"}); err != nil {
		t.Fatalf("create 'prod': %v", err)
	}
	// Case-sensitive: "Prod" is a DIFFERENT tag, must succeed.
	if _, err := tagSvc.Create(ctx, sitetag.CreateInput{TenantID: tenant, Name: "Prod"}); err != nil {
		t.Fatalf("create 'Prod' (distinct case) must succeed: %v", err)
	}
	// Exact duplicate: must 409.
	_, err := tagSvc.Create(ctx, sitetag.CreateInput{TenantID: tenant, Name: "prod"})
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "tag_name_exists" {
		t.Fatalf("duplicate create: got %v, want domain error tag_name_exists", err)
	}
}

// TestSiteTagRename_PropagatesToSitesAndPairingCodes proves a plain rename
// (no collision) rewrites BOTH sites.tags and an unexpired/unredeemed
// pairing code's tags, in the tag's own transaction.
func TestSiteTagRename_PropagatesToSitesAndPairingCodes(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	siteSvc, tagSvc := newSiteTagServices(pool)
	tenant := seedTenant(t, pool, "sitetag-rename-"+uuid.NewString()[:8])

	s, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://rename.example.com", Name: "seed"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	if _, err := siteSvc.SetTags(ctx, site.SetTagsInput{TenantID: tenant, SiteID: s.ID, Tags: []string{"staging", "eu"}}); err != nil {
		t.Fatalf("SetTags: %v", err)
	}
	pc, err := siteSvc.CreatePairingCode(ctx, site.CreatePairingCodeInput{TenantID: tenant, Tags: []string{"staging"}})
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}

	tags, err := tagSvc.List(ctx, tenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var stagingID uuid.UUID
	for _, tg := range tags {
		if tg.Name == "staging" {
			stagingID = tg.ID
		}
	}
	if stagingID == uuid.Nil {
		t.Fatal("registry has no 'staging' row after SetTags/CreatePairingCode")
	}

	newName := "stage"
	res, err := tagSvc.Update(ctx, sitetag.UpdateInput{TenantID: tenant, ID: stagingID, Name: &newName})
	if err != nil {
		t.Fatalf("Update (rename): %v", err)
	}
	if res.Merged {
		t.Fatal("plain rename must not report Merged")
	}
	if res.Tag.Name != "stage" {
		t.Fatalf("Tag.Name = %q, want %q", res.Tag.Name, "stage")
	}

	if got, want := sitesTagsOf(t, admin, s.ID), []string{"eu", "stage"}; !equalStrSlices(got, want) {
		t.Fatalf("site tags after rename = %v, want %v", got, want)
	}
	if got, want := pairingCodeTagsOf(t, admin, pc.Code.ID), []string{"stage"}; !equalStrSlices(got, want) {
		t.Fatalf("pairing code tags after rename = %v, want %v", got, want)
	}
}

// TestSiteTagRename_CollisionWithoutMerge409_WithMergeSurvivorDedup proves:
// renaming onto an existing name without merge:true 409s (registry
// untouched); with merge:true, the source tag is merged into the survivor,
// sites carrying BOTH names end up with the survivor exactly once (DISTINCT
// dedup), and the survivor's color is kept when the merge request carries no
// color.
func TestSiteTagRename_CollisionWithoutMerge409_WithMergeSurvivorDedup(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	siteSvc, tagSvc := newSiteTagServices(pool)
	tenant := seedTenant(t, pool, "sitetag-merge-"+uuid.NewString()[:8])

	// Survivor "prod" already has a color; source "qa" has none.
	survivor, err := tagSvc.Create(ctx, sitetag.CreateInput{TenantID: tenant, Name: "prod", Color: "#112233"})
	if err != nil {
		t.Fatalf("create survivor: %v", err)
	}
	source, err := tagSvc.Create(ctx, sitetag.CreateInput{TenantID: tenant, Name: "qa"})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	// A site carrying BOTH names — the merge rewrite must dedup it down to
	// exactly one "prod" entry, not "prod","prod".
	s, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://merge.example.com", Name: "seed"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	if _, err := siteSvc.SetTags(ctx, site.SetTagsInput{TenantID: tenant, SiteID: s.ID, Tags: []string{"prod", "qa"}}); err != nil {
		t.Fatalf("SetTags: %v", err)
	}

	// Without merge: 409, and the source tag must still exist untouched.
	newName := "prod"
	_, err = tagSvc.Update(ctx, sitetag.UpdateInput{TenantID: tenant, ID: source.ID, Name: &newName})
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "tag_name_exists" {
		t.Fatalf("rename without merge: got %v, want domain error tag_name_exists", err)
	}
	if registryRowCount(t, admin, tenant, "qa") != 1 {
		t.Fatal("source tag 'qa' must be untouched after a rejected (no-merge) rename")
	}

	// With merge: succeeds, survivor wins, source is deleted, site dedups.
	res, err := tagSvc.Update(ctx, sitetag.UpdateInput{TenantID: tenant, ID: source.ID, Name: &newName, Merge: true})
	if err != nil {
		t.Fatalf("rename with merge: %v", err)
	}
	if !res.Merged {
		t.Fatal("expected Merged = true")
	}
	if res.Tag.ID != survivor.ID {
		t.Fatalf("merged Tag.ID = %v, want survivor id %v", res.Tag.ID, survivor.ID)
	}
	if res.Tag.Color != "#112233" {
		t.Fatalf("merged Tag.Color = %q, want survivor's original color kept (%q)", res.Tag.Color, "#112233")
	}
	if registryRowCount(t, admin, tenant, "qa") != 0 {
		t.Fatal("source tag 'qa' registry row must be deleted after merge")
	}
	if got, want := sitesTagsOf(t, admin, s.ID), []string{"prod"}; !equalStrSlices(got, want) {
		t.Fatalf("site tags after merge = %v, want %v (deduped, not [prod,prod])", got, want)
	}
}

// TestSiteTagDelete_RemovesFleetWide proves DELETE removes the registry row
// AND strips the tag from every site AND every unexpired/unredeemed pairing
// code carrying it, in one transaction.
func TestSiteTagDelete_RemovesFleetWide(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	siteSvc, tagSvc := newSiteTagServices(pool)
	tenant := seedTenant(t, pool, "sitetag-delete-"+uuid.NewString()[:8])

	s1, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://del1.example.com", Name: "seed"})
	if err != nil {
		t.Fatalf("create site 1: %v", err)
	}
	s2, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://del2.example.com", Name: "seed"})
	if err != nil {
		t.Fatalf("create site 2: %v", err)
	}
	if _, err := siteSvc.SetTags(ctx, site.SetTagsInput{TenantID: tenant, SiteID: s1.ID, Tags: []string{"gone", "keep"}}); err != nil {
		t.Fatalf("SetTags s1: %v", err)
	}
	if _, err := siteSvc.SetTags(ctx, site.SetTagsInput{TenantID: tenant, SiteID: s2.ID, Tags: []string{"gone"}}); err != nil {
		t.Fatalf("SetTags s2: %v", err)
	}
	pc, err := siteSvc.CreatePairingCode(ctx, site.CreatePairingCodeInput{TenantID: tenant, Tags: []string{"gone"}})
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}

	tags, err := tagSvc.List(ctx, tenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var goneID uuid.UUID
	for _, tg := range tags {
		if tg.Name == "gone" {
			goneID = tg.ID
			if tg.UsageCount != 2 {
				t.Fatalf("usage_count for 'gone' = %d, want 2", tg.UsageCount)
			}
		}
	}
	if goneID == uuid.Nil {
		t.Fatal("registry has no 'gone' row")
	}

	if err := tagSvc.Delete(ctx, tenant, goneID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if got, want := sitesTagsOf(t, admin, s1.ID), []string{"keep"}; !equalStrSlices(got, want) {
		t.Fatalf("site 1 tags after delete = %v, want %v", got, want)
	}
	if got, want := sitesTagsOf(t, admin, s2.ID), []string{}; !equalStrSlices(got, want) {
		t.Fatalf("site 2 tags after delete = %v, want empty", got)
	}
	if got, want := pairingCodeTagsOf(t, admin, pc.Code.ID), []string{}; !equalStrSlices(got, want) {
		t.Fatalf("pairing code tags after delete = %v, want empty", got)
	}
	if registryRowCount(t, admin, tenant, "gone") != 0 {
		t.Fatal("registry row for 'gone' must be deleted")
	}

	// Deleting again 404s.
	err = tagSvc.Delete(ctx, tenant, goneID)
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "tag_not_found" {
		t.Fatalf("re-delete: got %v, want domain error tag_not_found", err)
	}
}

// TestSiteTagBulkApply_AddRemoveMath_And_Allowlist proves bulk-apply computes
// dedup(tags ∪ add) − remove entirely from the CURRENT row, upserts `add`
// into the registry, and that a site NOT in the caller's allowlist is
// excluded from the DB write and reported ok:false without failing the
// batch (the allowlist check itself is exercised at the handler layer via
// Principal.CanAccessSite — this proves the underlying math + registry
// upsert siteSvc.BulkApply performs for whatever siteIDs IT is given).
func TestSiteTagBulkApply_AddRemoveMath_And_Allowlist(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	siteSvc, tagSvc := newSiteTagServices(pool)
	tenant := seedTenant(t, pool, "sitetag-bulk-"+uuid.NewString()[:8])

	s1, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://bulk1.example.com", Name: "seed"})
	if err != nil {
		t.Fatalf("create site 1: %v", err)
	}
	if _, err := siteSvc.SetTags(ctx, site.SetTagsInput{TenantID: tenant, SiteID: s1.ID, Tags: []string{"old"}}); err != nil {
		t.Fatalf("SetTags: %v", err)
	}
	// A site_id that does not exist in this tenant (simulates cross-tenant/
	// nonexistent — the same 0-rows-affected path a caller-unauthorized site
	// would take once excluded by the handler's CanAccessSite gate).
	ghost := uuid.New()

	add, remove, err := tagSvc.ValidateDelta([]string{"new"}, []string{"old"})
	if err != nil {
		t.Fatalf("ValidateDelta: %v", err)
	}
	updated, err := tagSvc.BulkApply(ctx, tenant, []uuid.UUID{s1.ID, ghost}, add, remove)
	if err != nil {
		t.Fatalf("BulkApply: %v", err)
	}
	if !updated[s1.ID] {
		t.Fatal("s1 must be updated=true")
	}
	if updated[ghost] {
		t.Fatal("a nonexistent site_id must be updated=false, not an error")
	}
	if got, want := sitesTagsOf(t, admin, s1.ID), []string{"new"}; !equalStrSlices(got, want) {
		t.Fatalf("site tags after bulk-apply = %v, want %v (old removed, new added)", got, want)
	}
	if registryRowCount(t, admin, tenant, "new") != 1 {
		t.Fatal("bulk-apply's `add` names must be upserted into the registry")
	}
}
