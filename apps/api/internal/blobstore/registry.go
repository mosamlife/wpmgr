package blobstore

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/sitedestination"
)

// SnapshotLike is the minimal projection the Registry needs to route a presign
// to the right Store. We don't import the backup package here (cyclical), so
// the backup service passes us this slim shape.
type SnapshotLike struct {
	TenantID      uuid.UUID
	SiteID        uuid.UUID
	DestinationID uuid.UUID
}

// DestinationLookup is the bridge to the sitedestination service: hand back a
// destination row by id and the plaintext secret so we can build a Store. We
// hide the concrete service type behind an interface so the registry stays
// importable from the backup package without circling back to sitedestination.
type DestinationLookup interface {
	GetByID(ctx context.Context, tenantID, id uuid.UUID) (sitedestination.SiteDestination, error)
	GetDefaultForSite(ctx context.Context, tenantID, siteID uuid.UUID) (sitedestination.SiteDestination, error)
	DecryptSecret(d sitedestination.SiteDestination) (string, error)
}

// storeCacheKey scopes the Store cache by BOTH tenant and destination id.
// Defense-in-depth (security review, GH #146 Phase 1): destination_id is
// globally unique and every row is always same-tenant today, so keying by
// destination id alone was not exploitable — but a future feature that let a
// snapshot carry a cross-tenant destination_id (e.g. a snapshot clone/copy, or
// an admin re-point) would otherwise let a cache HIT hand back the WRONG
// tenant's bucket Store, since only the cache-MISS path re-validates the
// tenant via GetByID's InTenantTx + tenant_id filter. Keying by (tenantID,
// destID) makes a cross-tenant hit structurally impossible: it can only ever
// be a miss, which re-runs the tenant-scoped lookup.
type storeCacheKey struct {
	tenantID uuid.UUID
	destID   uuid.UUID
}

// Registry caches per-destination Stores so each customer-owned S3 bucket gets
// exactly one *Store across the process. The cache is keyed by
// (tenantID, destinationID) — see storeCacheKey — and built lazily on first
// use.
//
// The default Store is the CP-global bucket the API was booted with — every
// snapshot whose destination_id is uuid.Nil routes there, matching the legacy
// 0.9.6 behaviour.
type Registry struct {
	mu     sync.RWMutex
	stores map[storeCacheKey]*Store

	repo         DestinationLookup
	defaultStore *Store
}

// NewRegistry wires a Registry with the given default Store and destination
// lookup. The default Store may be nil (when the operator hasn't configured
// the CP-global S3 bucket); in that case every snapshot MUST carry a non-nil
// DestinationID or StoreForSnapshot will error.
func NewRegistry(defaultStore *Store, repo DestinationLookup) *Registry {
	return &Registry{
		stores:       make(map[storeCacheKey]*Store),
		repo:         repo,
		defaultStore: defaultStore,
	}
}

// DefaultStore returns the CP-global Store (or nil when none is configured).
func (r *Registry) DefaultStore() *Store { return r.defaultStore }

// StoreForSnapshot resolves the Store the presign service should use for the
// given snapshot. Three paths:
//
//   - snap.DestinationID is uuid.Nil    -> defaultStore (legacy CP bucket).
//   - destination row has Kind=cp       -> defaultStore.
//   - destination row has Kind=s3_compat-> build/cache a Store for the row.
//   - destination row has Kind=local    -> error; local destinations don't go
//     through presign (the agent writes bytes directly).
func (r *Registry) StoreForSnapshot(ctx context.Context, snap SnapshotLike) (*Store, error) {
	if snap.DestinationID == uuid.Nil {
		if r.defaultStore == nil {
			return nil, errors.New("blobstore registry: no default store configured")
		}
		return r.defaultStore, nil
	}

	// Cached path: avoid the DB round-trip + S3 client construction on every
	// presign. We hold a RWMutex so concurrent reads don't serialise. Keyed by
	// (tenantID, destinationID) — see storeCacheKey — so a cache HIT can never
	// cross a tenant boundary; only a cache MISS reaches GetByID, which is the
	// tenant-scoped re-validation.
	cacheKey := storeCacheKey{tenantID: snap.TenantID, destID: snap.DestinationID}
	r.mu.RLock()
	if store, ok := r.stores[cacheKey]; ok {
		r.mu.RUnlock()
		return store, nil
	}
	r.mu.RUnlock()

	if r.repo == nil {
		return nil, errors.New("blobstore registry: no destination lookup configured")
	}
	d, err := r.repo.GetByID(ctx, snap.TenantID, snap.DestinationID)
	if err != nil {
		return nil, fmt.Errorf("blobstore registry: lookup destination: %w", err)
	}

	switch d.Kind {
	case sitedestination.KindCP:
		if r.defaultStore == nil {
			return nil, errors.New("blobstore registry: cp destination but no default store configured")
		}
		return r.defaultStore, nil
	case sitedestination.KindLocal:
		return nil, errors.New("blobstore registry: local destinations do not use a Store (chunks go to disk on the agent)")
	case sitedestination.KindS3Compat:
		// fall through.
	default:
		return nil, fmt.Errorf("blobstore registry: unknown destination kind %q", d.Kind)
	}

	secret, err := r.repo.DecryptSecret(d)
	if err != nil {
		return nil, fmt.Errorf("blobstore registry: decrypt secret: %w", err)
	}

	// PathPrefix (GH #146 security review, functional gap): apply the
	// operator-configured key prefix to the Store itself so every PUT/GET
	// this Store mints — backup upload AND restore download alike — lands
	// under the SAME <path_prefix>/chunks/<tenant>/<hash> key, consistently.
	// backup_chunks.s3_key stays the bare canonical key regardless (dedup +
	// the tenant-scope assertion in mintRestoreChunks operate on that
	// unprefixed layer); the Store applies its prefix transparently at the
	// object-storage transport layer, so no caller needs to know about it.
	store, err := New(Config{
		Endpoint:       d.Endpoint,
		Region:         d.Region,
		Bucket:         d.Bucket,
		AccessKey:      d.AccessKeyID,
		SecretKey:      secret,
		ForcePathStyle: d.ForcePathStyle,
		PathPrefix:     d.PathPrefix,
	})
	if err != nil {
		return nil, fmt.Errorf("blobstore registry: build store: %w", err)
	}

	// Insert under the write lock — another goroutine may have raced in
	// between our read-unlock and this write-lock; resolve that by checking
	// once more under the write lock and reusing whichever Store landed first.
	r.mu.Lock()
	if existing, ok := r.stores[cacheKey]; ok {
		r.mu.Unlock()
		return existing, nil
	}
	r.stores[cacheKey] = store
	r.mu.Unlock()
	return store, nil
}

// Invalidate evicts a destination's cached Store. Called after the operator
// updates the credentials so the next presign re-fetches with the new key.
// tenantID must be the destination's own tenant — the cache is keyed by
// (tenantID, destinationID), see storeCacheKey.
func (r *Registry) Invalidate(tenantID, id uuid.UUID) {
	r.mu.Lock()
	delete(r.stores, storeCacheKey{tenantID: tenantID, destID: id})
	r.mu.Unlock()
}
