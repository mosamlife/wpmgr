package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// seedNamedUserRow inserts a users row directly (users is not RLS-scoped —
// see seedUserRow in admin_orphan_cleanup_integration_test.go for the same
// pattern) with an explicit display name, so the actor-name LEFT JOIN in
// ListAuditEntries/ListAuditEntriesFiltered has something real to resolve.
func seedNamedUserRow(t *testing.T, pool *db.Pool, email, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, name) VALUES ($1, $2) RETURNING id`, email, name).Scan(&id); err != nil {
		t.Fatalf("seed named user: %v", err)
	}
	return id
}

// TestAuditListSurvivesNonUUIDActorID proves the fix for the actor-name LEFT
// JOIN bug: a defensive `actor_id ~ regex AND id = actor_id::uuid` guard in a
// LEFT JOIN ON clause is INERT, because Postgres does not guarantee AND
// operands are evaluated left-to-right or short-circuited — the planner is
// free to evaluate the ::uuid cast before the regex guard for every row. Any
// tenant with a 'system' or otherwise non-uuid actor_id (written by uptime
// alerts, backups, scans, and update workers — see internal/uptime/alerts.go,
// internal/backup/worker.go, internal/scan/worker.go,
// internal/update/worker.go) 500s the whole /api/v1/audit page with
// "22P02: invalid input syntax for type uuid".
//
// Without the CASE-guarded cast in db/query/audit_log.sql, this test fails
// with a pgconn 22P02 error from both List and ListFiltered. With the fix, it
// passes: the non-uuid/empty actor_id rows resolve to NULL actor fields and a
// real actor_type='user' row still resolves its name + email.
func TestAuditListSurvivesNonUUIDActorID(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	tenant := seedTenant(t, pool, "audit-actor-join")
	userID := seedNamedUserRow(t, pool, "operator@audit-actor-join.example.com", "Operator Jane")

	// (a) a system-actor row with an EMPTY actor_id — exactly what
	// uptime/backup/scan/update workers write today.
	emptyActor, err := rec.Record(ctx, audit.Event{
		TenantID:  tenant,
		ActorType: audit.ActorSystem,
		ActorID:   "",
		Action:    "test.actor_join.empty",
	})
	if err != nil {
		t.Fatalf("record empty-actor row: %v", err)
	}

	// (b) a row whose actor_id is a non-empty, non-uuid string. The regex
	// guard must reject this before it ever reaches the ::uuid cast too.
	garbageActor, err := rec.Record(ctx, audit.Event{
		TenantID:  tenant,
		ActorType: audit.ActorSystem,
		ActorID:   "not-a-uuid",
		Action:    "test.actor_join.garbage",
	})
	if err != nil {
		t.Fatalf("record garbage-actor row: %v", err)
	}

	// (c) a normal actor_type='user' row with a real users.id that has a
	// name + email set, so the join must still resolve it correctly.
	userActor, err := rec.Record(ctx, audit.Event{
		TenantID:  tenant,
		ActorType: audit.ActorUser,
		ActorID:   userID.String(),
		Action:    "test.actor_join.user",
	})
	if err != nil {
		t.Fatalf("record user-actor row: %v", err)
	}

	// List must not error (this is the exact 500 the security review found).
	entries, err := rec.List(ctx, tenant, 100, 0)
	if err != nil {
		t.Fatalf("List errored — the actor-id LEFT JOIN cast bug is back: %v", err)
	}
	assertActorJoinResults(t, entries, emptyActor.ID, garbageActor.ID, userActor.ID, "Operator Jane", "operator@audit-actor-join.example.com")

	// ListFiltered shares the identical join contract — must not error either.
	filtered, err := rec.ListFiltered(ctx, tenant, audit.Filter{}, 100, 0)
	if err != nil {
		t.Fatalf("ListFiltered errored — the actor-id LEFT JOIN cast bug is back: %v", err)
	}
	assertActorJoinResults(t, filtered, emptyActor.ID, garbageActor.ID, userActor.ID, "Operator Jane", "operator@audit-actor-join.example.com")
}

func assertActorJoinResults(t *testing.T, entries []audit.Entry, emptyID, garbageID, userID uuid.UUID, wantName, wantEmail string) {
	t.Helper()
	byID := make(map[uuid.UUID]audit.Entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}

	empty, ok := byID[emptyID]
	if !ok {
		t.Fatalf("empty-actor entry %s not found in results", emptyID)
	}
	if empty.ActorName != nil {
		t.Errorf("empty-actor row: expected nil ActorName, got %q", *empty.ActorName)
	}
	if empty.ActorEmail != nil {
		t.Errorf("empty-actor row: expected nil ActorEmail, got %q", *empty.ActorEmail)
	}

	garbage, ok := byID[garbageID]
	if !ok {
		t.Fatalf("garbage-actor entry %s not found in results", garbageID)
	}
	if garbage.ActorName != nil {
		t.Errorf("garbage-actor row: expected nil ActorName, got %q", *garbage.ActorName)
	}
	if garbage.ActorEmail != nil {
		t.Errorf("garbage-actor row: expected nil ActorEmail, got %q", *garbage.ActorEmail)
	}

	user, ok := byID[userID]
	if !ok {
		t.Fatalf("user-actor entry %s not found in results", userID)
	}
	if user.ActorName == nil || *user.ActorName != wantName {
		t.Errorf("user-actor row: expected ActorName %q, got %v", wantName, user.ActorName)
	}
	if user.ActorEmail == nil || *user.ActorEmail != wantEmail {
		t.Errorf("user-actor row: expected ActorEmail %q, got %v", wantEmail, user.ActorEmail)
	}
}
