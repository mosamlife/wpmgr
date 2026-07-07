package blobstore

// registry_test.go — ADR-036 P1 (GH #146) unit tests for the presign-routing
// Registry: an s3_compat destination must route to a Store built against the
// CUSTOMER's own bucket (never the CP-managed default), a nil/cp destination
// must route to the default Store, and a local destination must error (the
// agent writes chunks to disk itself; the CP never presigns for it).
//
// blobstore.New performs no network I/O, so these tests need no real S3
// endpoint — only a fake DestinationLookup.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/sitedestination"
)

// fakeDestLookup is a minimal DestinationLookup double: GetByID/GetDefaultForSite
// return a canned row per id; DecryptSecret returns a fixed plaintext without
// touching age at all (blobstore.New never performs network I/O, so no real
// credentials are required to prove routing picked the right bucket).
type fakeDestLookup struct {
	byID map[uuid.UUID]sitedestination.SiteDestination
}

var errFakeNotFound = errors.New("fake destination lookup: not found")

// GetByID mirrors the real Repo.GetByID's tenant scoping (RLS + an explicit
// `WHERE id=$1 AND tenant_id=$2`): a row only resolves when BOTH the id AND
// the requested tenantID match. This is load-bearing for
// TestRegistry_StoreForSnapshot_CacheIsTenantScoped — a fake that ignored
// tenantID here would never be able to prove the cache-miss path re-validates
// the tenant.
func (f *fakeDestLookup) GetByID(_ context.Context, tenantID, id uuid.UUID) (sitedestination.SiteDestination, error) {
	d, ok := f.byID[id]
	if !ok || d.TenantID != tenantID {
		return sitedestination.SiteDestination{}, errFakeNotFound
	}
	return d, nil
}

func (f *fakeDestLookup) GetDefaultForSite(_ context.Context, _, _ uuid.UUID) (sitedestination.SiteDestination, error) {
	return sitedestination.SiteDestination{}, errFakeNotFound
}

func (f *fakeDestLookup) DecryptSecret(_ sitedestination.SiteDestination) (string, error) {
	return "plaintext-secret", nil
}

func TestRegistry_StoreForSnapshot_S3CompatRoutesToCustomerBucket(t *testing.T) {
	defaultStore, err := New(Config{Bucket: "cp-managed-bucket"})
	if err != nil {
		t.Fatalf("default store: %v", err)
	}
	tenantID := uuid.New()
	destID := uuid.New()
	lookup := &fakeDestLookup{byID: map[uuid.UUID]sitedestination.SiteDestination{
		destID: {
			ID:          destID,
			TenantID:    tenantID,
			Kind:        sitedestination.KindS3Compat,
			Bucket:      "customer-owned-bucket",
			Region:      "us-east-1",
			AccessKeyID: "AKIAEXAMPLE",
		},
	}}
	registry := NewRegistry(defaultStore, lookup)

	store, err := registry.StoreForSnapshot(context.Background(), SnapshotLike{
		TenantID:      tenantID,
		SiteID:        uuid.New(),
		DestinationID: destID,
	})
	if err != nil {
		t.Fatalf("StoreForSnapshot: %v", err)
	}
	if store == defaultStore {
		t.Fatal("expected the customer-specific store, got the default (CP-managed) store")
	}
	if store.Bucket() != "customer-owned-bucket" {
		t.Errorf("Bucket() = %q, want %q", store.Bucket(), "customer-owned-bucket")
	}

	// A second call for the same destination must hit the cache (same *Store),
	// not rebuild — StoreForSnapshot's doc promises exactly one Store per
	// destination ID across the process.
	store2, err := registry.StoreForSnapshot(context.Background(), SnapshotLike{
		TenantID: tenantID, SiteID: uuid.New(), DestinationID: destID,
	})
	if err != nil {
		t.Fatalf("StoreForSnapshot (cached): %v", err)
	}
	if store2 != store {
		t.Error("expected the cached Store on a second call for the same destination id")
	}
}

func TestRegistry_StoreForSnapshot_NilDestinationUsesDefault(t *testing.T) {
	defaultStore, err := New(Config{Bucket: "cp-managed-bucket"})
	if err != nil {
		t.Fatalf("default store: %v", err)
	}
	registry := NewRegistry(defaultStore, &fakeDestLookup{byID: map[uuid.UUID]sitedestination.SiteDestination{}})

	store, err := registry.StoreForSnapshot(context.Background(), SnapshotLike{
		TenantID:      uuid.New(),
		SiteID:        uuid.New(),
		DestinationID: uuid.Nil,
	})
	if err != nil {
		t.Fatalf("StoreForSnapshot: %v", err)
	}
	if store != defaultStore {
		t.Error("expected the default store for a nil destination id")
	}
}

func TestRegistry_StoreForSnapshot_CPKindUsesDefault(t *testing.T) {
	defaultStore, err := New(Config{Bucket: "cp-managed-bucket"})
	if err != nil {
		t.Fatalf("default store: %v", err)
	}
	tenantID := uuid.New()
	destID := uuid.New()
	lookup := &fakeDestLookup{byID: map[uuid.UUID]sitedestination.SiteDestination{
		destID: {ID: destID, TenantID: tenantID, Kind: sitedestination.KindCP},
	}}
	registry := NewRegistry(defaultStore, lookup)

	store, err := registry.StoreForSnapshot(context.Background(), SnapshotLike{
		TenantID: tenantID, SiteID: uuid.New(), DestinationID: destID,
	})
	if err != nil {
		t.Fatalf("StoreForSnapshot: %v", err)
	}
	if store != defaultStore {
		t.Error("expected the default store for a kind=cp destination row")
	}
}

func TestRegistry_StoreForSnapshot_LocalErrors(t *testing.T) {
	defaultStore, err := New(Config{Bucket: "cp-managed-bucket"})
	if err != nil {
		t.Fatalf("default store: %v", err)
	}
	tenantID := uuid.New()
	destID := uuid.New()
	lookup := &fakeDestLookup{byID: map[uuid.UUID]sitedestination.SiteDestination{
		destID: {ID: destID, TenantID: tenantID, Kind: sitedestination.KindLocal},
	}}
	registry := NewRegistry(defaultStore, lookup)

	if _, err := registry.StoreForSnapshot(context.Background(), SnapshotLike{
		TenantID: tenantID, SiteID: uuid.New(), DestinationID: destID,
	}); err == nil {
		t.Fatal("expected an error for a local destination (agent writes to disk; the CP never presigns for it)")
	}
}

// TestRegistry_StoreForSnapshot_CacheIsTenantScoped is the security-review
// (GH #146 Phase 1, LOW defense-in-depth) regression proof: the Store cache
// is keyed by (tenantID, destinationID), so a cache HIT can never cross a
// tenant boundary — only a cache MISS is possible for an unfamiliar tenant,
// and a miss always re-runs the tenant-scoped GetByID. destination_id is
// globally unique and always same-tenant today, so this scenario (the same
// destID requested under two different tenants) cannot occur via the real
// repo — this test simulates it directly against the Registry to prove the
// cache key itself enforces the invariant, independent of whether the
// scenario is reachable today.
func TestRegistry_StoreForSnapshot_CacheIsTenantScoped(t *testing.T) {
	defaultStore, err := New(Config{Bucket: "cp-managed-bucket"})
	if err != nil {
		t.Fatalf("default store: %v", err)
	}
	tenantA := uuid.New()
	tenantB := uuid.New()
	destID := uuid.New()

	// The fake lookup only recognises destID under tenantA — mirroring the
	// real GetByID's tenant-scoped WHERE clause (a destination row belongs to
	// exactly one tenant).
	lookup := &fakeDestLookup{byID: map[uuid.UUID]sitedestination.SiteDestination{
		destID: {ID: destID, TenantID: tenantA, Kind: sitedestination.KindS3Compat, Bucket: "tenant-a-bucket", AccessKeyID: "AKIA"},
	}}
	registry := NewRegistry(defaultStore, lookup)

	// First call, tenantA: resolves + caches under (tenantA, destID).
	storeA, err := registry.StoreForSnapshot(context.Background(), SnapshotLike{
		TenantID: tenantA, SiteID: uuid.New(), DestinationID: destID,
	})
	if err != nil {
		t.Fatalf("StoreForSnapshot (tenantA): %v", err)
	}
	if storeA.Bucket() != "tenant-a-bucket" {
		t.Fatalf("Bucket() = %q, want tenant-a-bucket", storeA.Bucket())
	}

	// Second call, the SAME destID but tenantB. If the cache were keyed by
	// destID alone, this would return storeA (tenantA's bucket!) WITHOUT ever
	// re-checking the tenant — the exact gap the security review flagged.
	// Keyed by (tenantID, destID), this MUST miss the cache and re-run
	// GetByID, which fails for tenantB (the fake correctly models the
	// destination belonging only to tenantA).
	if _, err := registry.StoreForSnapshot(context.Background(), SnapshotLike{
		TenantID: tenantB, SiteID: uuid.New(), DestinationID: destID,
	}); err == nil {
		t.Fatal("expected an error for tenantB (destination belongs to tenantA) — the cache must not leak tenantA's store across the tenant boundary")
	}
}

// TestRegistry_StoreForSnapshot_PathPrefixApplied is the functional-gap fix
// proof: an s3_compat destination's configured path_prefix must be applied to
// the Store it builds, so every key the Store touches lands under
// <path_prefix>/<key> instead of the bucket root.
func TestRegistry_StoreForSnapshot_PathPrefixApplied(t *testing.T) {
	defaultStore, err := New(Config{Bucket: "cp-managed-bucket"})
	if err != nil {
		t.Fatalf("default store: %v", err)
	}
	tenantID := uuid.New()
	destID := uuid.New()
	lookup := &fakeDestLookup{byID: map[uuid.UUID]sitedestination.SiteDestination{
		destID: {
			ID:          destID,
			TenantID:    tenantID,
			Kind:        sitedestination.KindS3Compat,
			Bucket:      "customer-owned-bucket",
			PathPrefix:  "/clientA/backups/",
			AccessKeyID: "AKIAEXAMPLE",
		},
	}}
	registry := NewRegistry(defaultStore, lookup)

	store, err := registry.StoreForSnapshot(context.Background(), SnapshotLike{
		TenantID: tenantID, SiteID: uuid.New(), DestinationID: destID,
	})
	if err != nil {
		t.Fatalf("StoreForSnapshot: %v", err)
	}
	if store.PathPrefix() != "clientA/backups" {
		t.Errorf("PathPrefix() = %q, want %q (leading/trailing slashes normalised)", store.PathPrefix(), "clientA/backups")
	}
}
