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

// Registry caches per-destination Stores so each customer-owned S3 bucket gets
// exactly one *Store across the process. The cache is keyed by destination ID
// (a stable UUID) and built lazily on first use.
//
// The default Store is the CP-global bucket the API was booted with — every
// snapshot whose destination_id is uuid.Nil routes there, matching the legacy
// 0.9.6 behaviour.
type Registry struct {
	mu     sync.RWMutex
	stores map[uuid.UUID]*Store

	repo         DestinationLookup
	defaultStore *Store
}

// NewRegistry wires a Registry with the given default Store and destination
// lookup. The default Store may be nil (when the operator hasn't configured
// the CP-global S3 bucket); in that case every snapshot MUST carry a non-nil
// DestinationID or StoreForSnapshot will error.
func NewRegistry(defaultStore *Store, repo DestinationLookup) *Registry {
	return &Registry{
		stores:       make(map[uuid.UUID]*Store),
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
	// presign. We hold a RWMutex so concurrent reads don't serialise.
	r.mu.RLock()
	if store, ok := r.stores[snap.DestinationID]; ok {
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

	store, err := New(Config{
		Endpoint:       d.Endpoint,
		Region:         d.Region,
		Bucket:         d.Bucket,
		AccessKey:      d.AccessKeyID,
		SecretKey:      secret,
		ForcePathStyle: d.ForcePathStyle,
	})
	if err != nil {
		return nil, fmt.Errorf("blobstore registry: build store: %w", err)
	}

	// Insert under the write lock — another goroutine may have raced in
	// between our read-unlock and this write-lock; resolve that by checking
	// once more under the write lock and reusing whichever Store landed first.
	r.mu.Lock()
	if existing, ok := r.stores[d.ID]; ok {
		r.mu.Unlock()
		return existing, nil
	}
	r.stores[d.ID] = store
	r.mu.Unlock()
	return store, nil
}

// Invalidate evicts a destination's cached Store. Called after the operator
// updates the credentials so the next presign re-fetches with the new key.
func (r *Registry) Invalidate(id uuid.UUID) {
	r.mu.Lock()
	delete(r.stores, id)
	r.mu.Unlock()
}
