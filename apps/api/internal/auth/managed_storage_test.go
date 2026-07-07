package auth

// managed_storage_test.go — M16 Phase B Me.managed_storage_allowed tests.
//
// White-box, in-memory; no database. Mirrors the fail-open contract every
// other billing-adjacent optional dependency in this codebase uses (nil
// resolver / nil tenant / resolver error all resolve to true, matching
// Unlimited().ManagedBackupStorage's self-host default).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeManagedStorageResolver is an in-memory ManagedStorageResolver double.
type fakeManagedStorageResolver struct {
	allowed bool
	err     error
	calls   []uuid.UUID
}

func (f *fakeManagedStorageResolver) ManagedStorageAllowed(_ context.Context, tenantID uuid.UUID) (bool, error) {
	f.calls = append(f.calls, tenantID)
	return f.allowed, f.err
}

func TestManagedStorageAllowed_NilResolverIsTrue(t *testing.T) {
	h := &Handler{}
	if !h.managedStorageAllowed(context.Background(), uuid.New()) {
		t.Fatal("expected true when no ManagedStorageResolver is wired")
	}
}

func TestManagedStorageAllowed_NilTenantIsTrue(t *testing.T) {
	resolver := &fakeManagedStorageResolver{allowed: false}
	h := &Handler{managedStorage: resolver}
	if !h.managedStorageAllowed(context.Background(), uuid.Nil) {
		t.Fatal("expected true for a nil (no active org yet) tenant, regardless of the resolver's answer")
	}
	if len(resolver.calls) != 0 {
		t.Fatal("expected the resolver to never be called for a nil tenant")
	}
}

func TestManagedStorageAllowed_ResolverErrorFailsOpen(t *testing.T) {
	resolver := &fakeManagedStorageResolver{err: errors.New("boom")}
	h := &Handler{managedStorage: resolver}
	if !h.managedStorageAllowed(context.Background(), uuid.New()) {
		t.Fatal("expected true (fail open) when the resolver errors — this is a display signal, never a gate")
	}
}

func TestManagedStorageAllowed_PropagatesResolverAnswer(t *testing.T) {
	tenantID := uuid.New()
	resolver := &fakeManagedStorageResolver{allowed: false}
	h := &Handler{managedStorage: resolver}
	if h.managedStorageAllowed(context.Background(), tenantID) {
		t.Fatal("expected false to propagate from the resolver for a real, resolvable tenant")
	}
	if len(resolver.calls) != 1 || resolver.calls[0] != tenantID {
		t.Fatalf("expected exactly 1 resolver call for tenant %v, got %v", tenantID, resolver.calls)
	}
}

// ---- toMe --------------------------------------------------------------

func TestToMe_SetsManagedStorageAllowed(t *testing.T) {
	me := toMe(User{}, nil, uuid.New(), true, false)
	if !me.Hosted.Value {
		t.Fatal("expected Hosted to propagate through toMe")
	}
	if !me.ManagedStorageAllowed.Set || me.ManagedStorageAllowed.Value {
		t.Fatalf("expected ManagedStorageAllowed = false (set), got %+v", me.ManagedStorageAllowed)
	}

	me2 := toMe(User{}, nil, uuid.New(), true, true)
	if !me2.ManagedStorageAllowed.Set || !me2.ManagedStorageAllowed.Value {
		t.Fatalf("expected ManagedStorageAllowed = true (set), got %+v", me2.ManagedStorageAllowed)
	}
}
