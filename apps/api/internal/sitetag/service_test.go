package sitetag

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// stubRepo is a no-op Repo used to test service validation without a real
// DB. Individual tests override the func fields they care about.
type stubRepo struct {
	createFn    func(ctx context.Context, in CreateInput) (Tag, error)
	updateFn    func(ctx context.Context, in UpdateInput) (UpdateResult, error)
	deleteFn    func(ctx context.Context, tenantID, id uuid.UUID) error
	bulkApplyFn func(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, add, remove []string) (map[uuid.UUID]bool, error)
}

func (r *stubRepo) List(_ context.Context, _ uuid.UUID) ([]Tag, error) { return nil, nil }

func (r *stubRepo) Create(ctx context.Context, in CreateInput) (Tag, error) {
	if r.createFn != nil {
		return r.createFn(ctx, in)
	}
	return Tag{TenantID: in.TenantID, Name: in.Name, Color: in.Color}, nil
}

func (r *stubRepo) Update(ctx context.Context, in UpdateInput) (UpdateResult, error) {
	if r.updateFn != nil {
		return r.updateFn(ctx, in)
	}
	return UpdateResult{}, nil
}

func (r *stubRepo) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	if r.deleteFn != nil {
		return r.deleteFn(ctx, tenantID, id)
	}
	return nil
}

func (r *stubRepo) BulkApply(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, add, remove []string) (map[uuid.UUID]bool, error) {
	if r.bulkApplyFn != nil {
		return r.bulkApplyFn(ctx, tenantID, siteIDs, add, remove)
	}
	out := make(map[uuid.UUID]bool, len(siteIDs))
	for _, id := range siteIDs {
		out[id] = true
	}
	return out, nil
}

func newTestService(repo *stubRepo) *Service {
	if repo == nil {
		repo = &stubRepo{}
	}
	return NewService(repo)
}

func assertDomainCode(t *testing.T, err error, want string) {
	t.Helper()
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error with code %q, got %v", want, err)
	}
	if de.Code != want {
		t.Fatalf("code = %q, want %q", de.Code, want)
	}
}

// ---------------------------------------------------------------------------
// Create validation
// ---------------------------------------------------------------------------

func TestCreateRequiresTenant(t *testing.T) {
	svc := newTestService(nil)
	_, err := svc.Create(context.Background(), CreateInput{TenantID: uuid.Nil, Name: "prod"})
	assertDomainCode(t, err, "tenant_required")
}

func TestCreateBlankNameRejected(t *testing.T) {
	svc := newTestService(nil)
	_, err := svc.Create(context.Background(), CreateInput{TenantID: uuid.New(), Name: "   "})
	assertDomainCode(t, err, "invalid_tag")
}

func TestCreateNameTooLongRejected(t *testing.T) {
	svc := newTestService(nil)
	long := ""
	for i := 0; i < 65; i++ {
		long += "a"
	}
	_, err := svc.Create(context.Background(), CreateInput{TenantID: uuid.New(), Name: long})
	assertDomainCode(t, err, "invalid_tag")
}

func TestCreateInvalidColorRejected(t *testing.T) {
	svc := newTestService(nil)
	_, err := svc.Create(context.Background(), CreateInput{TenantID: uuid.New(), Name: "prod", Color: "blue"})
	assertDomainCode(t, err, "invalid_color")
}

func TestCreateNormalizesColorToLowercase(t *testing.T) {
	repo := &stubRepo{}
	svc := newTestService(repo)
	tag, err := svc.Create(context.Background(), CreateInput{TenantID: uuid.New(), Name: "prod", Color: "#ABCDEF"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tag.Color != "#abcdef" {
		t.Fatalf("Color = %q, want lowercase #abcdef", tag.Color)
	}
}

func TestCreateTrimsName(t *testing.T) {
	repo := &stubRepo{}
	svc := newTestService(repo)
	tag, err := svc.Create(context.Background(), CreateInput{TenantID: uuid.New(), Name: "  prod  "})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tag.Name != "prod" {
		t.Fatalf("Name = %q, want trimmed %q", tag.Name, "prod")
	}
}

// TestCreateDuplicateNameConflict proves the repo's unique_violation mapping
// (exercised for real against Postgres in the integration test) is surfaced
// unchanged through the service layer.
func TestCreateDuplicateNameConflict(t *testing.T) {
	repo := &stubRepo{
		createFn: func(ctx context.Context, in CreateInput) (Tag, error) {
			return Tag{}, domain.Conflict("tag_name_exists", "a tag with this name already exists")
		},
	}
	svc := newTestService(repo)
	_, err := svc.Create(context.Background(), CreateInput{TenantID: uuid.New(), Name: "prod"})
	assertDomainCode(t, err, "tag_name_exists")
}

// TestCreatePreservesCase proves the service does not lowercase/normalize
// tag names beyond trimming — "Prod" and "prod" are distinct case-sensitive
// names (matching site.normalizeTags' `= ANY(tags)` semantics).
func TestCreatePreservesCase(t *testing.T) {
	repo := &stubRepo{}
	svc := newTestService(repo)
	tag, err := svc.Create(context.Background(), CreateInput{TenantID: uuid.New(), Name: "Prod"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tag.Name != "Prod" {
		t.Fatalf("Name = %q, want case preserved %q", tag.Name, "Prod")
	}
}

// ---------------------------------------------------------------------------
// Update validation
// ---------------------------------------------------------------------------

func TestUpdateRequiresTenant(t *testing.T) {
	svc := newTestService(nil)
	_, err := svc.Update(context.Background(), UpdateInput{TenantID: uuid.Nil, ID: uuid.New()})
	assertDomainCode(t, err, "tenant_required")
}

func TestUpdateInvalidNameRejected(t *testing.T) {
	svc := newTestService(nil)
	blank := "   "
	_, err := svc.Update(context.Background(), UpdateInput{TenantID: uuid.New(), ID: uuid.New(), Name: &blank})
	assertDomainCode(t, err, "invalid_tag")
}

func TestUpdateInvalidColorRejected(t *testing.T) {
	svc := newTestService(nil)
	bad := "not-a-color"
	_, err := svc.Update(context.Background(), UpdateInput{TenantID: uuid.New(), ID: uuid.New(), Color: &bad})
	assertDomainCode(t, err, "invalid_color")
}

func TestUpdateRenameCollisionWithoutMergeConflict(t *testing.T) {
	repo := &stubRepo{
		updateFn: func(ctx context.Context, in UpdateInput) (UpdateResult, error) {
			if !in.Merge {
				return UpdateResult{}, domain.Conflict("tag_name_exists", "a tag with this name already exists")
			}
			return UpdateResult{Tag: Tag{Name: *in.Name}, Merged: true, OldName: "old", MergedInto: *in.Name}, nil
		},
	}
	svc := newTestService(repo)
	newName := "existing"
	_, err := svc.Update(context.Background(), UpdateInput{TenantID: uuid.New(), ID: uuid.New(), Name: &newName})
	assertDomainCode(t, err, "tag_name_exists")
}

func TestUpdateRenameCollisionWithMergeSucceeds(t *testing.T) {
	repo := &stubRepo{
		updateFn: func(ctx context.Context, in UpdateInput) (UpdateResult, error) {
			if !in.Merge {
				return UpdateResult{}, domain.Conflict("tag_name_exists", "a tag with this name already exists")
			}
			return UpdateResult{Tag: Tag{Name: *in.Name}, Merged: true, OldName: "old", MergedInto: *in.Name}, nil
		},
	}
	svc := newTestService(repo)
	newName := "existing"
	res, err := svc.Update(context.Background(), UpdateInput{TenantID: uuid.New(), ID: uuid.New(), Name: &newName, Merge: true})
	if err != nil {
		t.Fatalf("Update with merge: %v", err)
	}
	if !res.Merged {
		t.Fatal("expected Merged = true")
	}
	if res.MergedInto != newName {
		t.Fatalf("MergedInto = %q, want %q", res.MergedInto, newName)
	}
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestDeleteRequiresTenant(t *testing.T) {
	svc := newTestService(nil)
	err := svc.Delete(context.Background(), uuid.Nil, uuid.New())
	assertDomainCode(t, err, "tenant_required")
}

func TestDeleteNotFound(t *testing.T) {
	repo := &stubRepo{
		deleteFn: func(ctx context.Context, tenantID, id uuid.UUID) error {
			return domain.NotFound("tag_not_found", "tag not found")
		},
	}
	svc := newTestService(repo)
	err := svc.Delete(context.Background(), uuid.New(), uuid.New())
	assertDomainCode(t, err, "tag_not_found")
}

// ---------------------------------------------------------------------------
// BulkApply / ValidateDelta
// ---------------------------------------------------------------------------

func TestValidateDeltaRequiresNonEmpty(t *testing.T) {
	svc := newTestService(nil)
	_, _, err := svc.ValidateDelta(nil, nil)
	assertDomainCode(t, err, "tag_delta_required")
}

func TestValidateDeltaBlankEntriesDropped(t *testing.T) {
	svc := newTestService(nil)
	add, remove, err := svc.ValidateDelta([]string{"  ", "prod"}, []string{" "})
	if err != nil {
		t.Fatalf("ValidateDelta: %v", err)
	}
	if len(add) != 1 || add[0] != "prod" {
		t.Fatalf("add = %v, want [prod]", add)
	}
	if len(remove) != 0 {
		t.Fatalf("remove = %v, want empty (blank-only entries dropped)", remove)
	}
}

func TestValidateDeltaNameTooLongRejected(t *testing.T) {
	svc := newTestService(nil)
	long := ""
	for i := 0; i < 65; i++ {
		long += "a"
	}
	_, _, err := svc.ValidateDelta([]string{long}, nil)
	assertDomainCode(t, err, "invalid_tag")
}

func TestBulkApplyEmptySiteIDsIsNoop(t *testing.T) {
	svc := newTestService(&stubRepo{
		bulkApplyFn: func(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, add, remove []string) (map[uuid.UUID]bool, error) {
			t.Fatal("repo.BulkApply must not be called for an empty siteIDs slice")
			return nil, nil
		},
	})
	got, err := svc.BulkApply(context.Background(), uuid.New(), nil, []string{"prod"}, nil)
	if err != nil {
		t.Fatalf("BulkApply: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d results, want 0 (no authorized sites)", len(got))
	}
}

func TestBulkApplyRequiresTenant(t *testing.T) {
	svc := newTestService(nil)
	_, err := svc.BulkApply(context.Background(), uuid.Nil, []uuid.UUID{uuid.New()}, []string{"prod"}, nil)
	assertDomainCode(t, err, "tenant_required")
}

// TestBulkApplyAppliedMap proves the service passes through the repo's
// per-site updated map unchanged — the add/remove SQL-side math itself is
// covered by the Postgres integration test (ApplyTagDeltaToSite runs real
// array arithmetic that a stub cannot exercise).
func TestBulkApplyAppliedMap(t *testing.T) {
	site1, site2 := uuid.New(), uuid.New()
	repo := &stubRepo{
		bulkApplyFn: func(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, add, remove []string) (map[uuid.UUID]bool, error) {
			return map[uuid.UUID]bool{site1: true, site2: false}, nil
		},
	}
	svc := newTestService(repo)
	got, err := svc.BulkApply(context.Background(), uuid.New(), []uuid.UUID{site1, site2}, []string{"a"}, nil)
	if err != nil {
		t.Fatalf("BulkApply: %v", err)
	}
	if !got[site1] || got[site2] {
		t.Fatalf("got = %v, want {site1:true, site2:false}", got)
	}
}
