package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestAuditFilterBySiteID_MatchesMetadataSiteID is GH #201: backup/restore/
// update lifecycle audit rows target a snapshot/run/task id (target_type !=
// "site"), so the pre-fix site_id filter (which only matched
// target_type='site' AND target_id=site_id) returned ZERO rows for them even
// though the recorder always stamps metadata.site_id. Confirms the broadened
// predicate now matches on metadata.site_id for a non-site target_type.
func TestAuditFilterBySiteID_MatchesMetadataSiteID(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	tenant := seedTenant(t, pool, "audit-site-filter-meta")
	siteID := uuid.New()
	snapshotID := uuid.New()

	entry, err := rec.Record(ctx, audit.Event{
		TenantID:   tenant,
		ActorType:  audit.ActorSystem,
		ActorID:    "",
		Action:     "backup.snapshot.completed",
		TargetType: "backup_snapshot",
		TargetID:   snapshotID.String(),
		Metadata:   map[string]any{"site_id": siteID.String(), "kind": "full"},
	})
	if err != nil {
		t.Fatalf("record backup snapshot event: %v", err)
	}

	// A different site's event must NOT be returned.
	other, err := rec.Record(ctx, audit.Event{
		TenantID:   tenant,
		ActorType:  audit.ActorSystem,
		ActorID:    "",
		Action:     "backup.snapshot.completed",
		TargetType: "backup_snapshot",
		TargetID:   uuid.New().String(),
		Metadata:   map[string]any{"site_id": uuid.New().String(), "kind": "full"},
	})
	if err != nil {
		t.Fatalf("record other-site backup snapshot event: %v", err)
	}

	got, err := rec.ListFiltered(ctx, tenant, audit.Filter{SiteID: &siteID}, 100, 0)
	if err != nil {
		t.Fatalf("ListFiltered by site_id errored: %v", err)
	}

	byID := make(map[uuid.UUID]audit.Entry, len(got))
	for _, e := range got {
		byID[e.ID] = e
	}
	if _, ok := byID[entry.ID]; !ok {
		t.Errorf("expected backup_snapshot row (metadata.site_id match) in filtered results, got %d rows", len(got))
	}
	if _, ok := byID[other.ID]; ok {
		t.Errorf("other site's backup_snapshot row must not be returned when filtering by a different site_id")
	}
}

// TestAuditFilterBySiteID_MatchesBackupScheduleTargetID is GH #201's third
// predicate shape: backup.schedule.changed rows carry the site id in
// target_id (target_type="backup_schedule") but never write metadata.site_id
// (see backup.Handler.recordScheduleChange) — they must still be matched.
func TestAuditFilterBySiteID_MatchesBackupScheduleTargetID(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	tenant := seedTenant(t, pool, "audit-site-filter-sched")
	siteID := uuid.New()

	entry, err := rec.Record(ctx, audit.Event{
		TenantID:   tenant,
		ActorType:  audit.ActorUser,
		ActorID:    uuid.New().String(),
		Action:     "backup.schedule.changed",
		TargetType: "backup_schedule",
		TargetID:   siteID.String(),
		Metadata:   map[string]any{"cadence": "daily", "kind": "full", "enabled": true},
	})
	if err != nil {
		t.Fatalf("record backup_schedule event: %v", err)
	}

	got, err := rec.ListFiltered(ctx, tenant, audit.Filter{SiteID: &siteID}, 100, 0)
	if err != nil {
		t.Fatalf("ListFiltered by site_id errored: %v", err)
	}
	found := false
	for _, e := range got {
		if e.ID == entry.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected backup_schedule row (target_id match, no metadata.site_id) in filtered results, got %d rows", len(got))
	}
}

// TestAuditFilterBySiteID_MalformedOrAbsentMetadataSiteIDExcludedNoError
// proves the metadata.site_id predicate is TEXT-to-TEXT and never throws: a
// row whose metadata.site_id is malformed (not a valid UUID) or entirely
// absent must be silently excluded from a site_id-filtered query, never a
// 22P02 "invalid input syntax for type uuid" error — the same failure class
// the CASE-guarded actor-id join exists to avoid (see audit_actor_join_test.go).
func TestAuditFilterBySiteID_MalformedOrAbsentMetadataSiteIDExcludedNoError(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	tenant := seedTenant(t, pool, "audit-site-filter-malformed")
	siteID := uuid.New()

	malformed, err := rec.Record(ctx, audit.Event{
		TenantID:   tenant,
		ActorType:  audit.ActorSystem,
		ActorID:    "",
		Action:     "update.task.failed",
		TargetType: "update_task",
		TargetID:   uuid.New().String(),
		Metadata:   map[string]any{"site_id": "not-a-uuid"},
	})
	if err != nil {
		t.Fatalf("record malformed-metadata event: %v", err)
	}

	absent, err := rec.Record(ctx, audit.Event{
		TenantID:   tenant,
		ActorType:  audit.ActorSystem,
		ActorID:    "",
		Action:     "update.run.created",
		TargetType: "update_run",
		TargetID:   uuid.New().String(),
		Metadata:   map[string]any{"reason": "scheduled"}, // no site_id key at all
	})
	if err != nil {
		t.Fatalf("record no-site-id-metadata event: %v", err)
	}

	got, err := rec.ListFiltered(ctx, tenant, audit.Filter{SiteID: &siteID}, 100, 0)
	if err != nil {
		t.Fatalf("ListFiltered must not error on malformed/absent metadata.site_id, got: %v", err)
	}
	for _, e := range got {
		if e.ID == malformed.ID {
			t.Errorf("malformed metadata.site_id row must not match an unrelated site_id filter")
		}
		if e.ID == absent.ID {
			t.Errorf("row with no metadata.site_id key must not match a site_id filter")
		}
	}

	// The unfiltered list must still surface both rows untouched — the fix
	// only changes the filtered predicate, never the stored data.
	all, err := rec.List(ctx, tenant, 100, 0)
	if err != nil {
		t.Fatalf("List errored: %v", err)
	}
	allByID := make(map[uuid.UUID]audit.Entry, len(all))
	for _, e := range all {
		allByID[e.ID] = e
	}
	if _, ok := allByID[malformed.ID]; !ok {
		t.Errorf("malformed-metadata row missing from unfiltered List")
	}
	if _, ok := allByID[absent.ID]; !ok {
		t.Errorf("absent-metadata row missing from unfiltered List")
	}
}

// TestAuditFilterBySiteID_TenantIsolationHolds confirms the broadened site_id
// predicate never weakens tenant isolation: two tenants each record a
// backup-lifecycle row whose metadata.site_id happens to be the SAME value
// (a plausible UUID collision is astronomically unlikely, but nothing in the
// new OR-branches should ever let it cross tenants regardless — al.tenant_id
// = @tenant_id gates the whole predicate, and RLS is the independent
// second gate).
func TestAuditFilterBySiteID_TenantIsolationHolds(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	tenantA := seedTenant(t, pool, "audit-site-filter-tenant-a")
	tenantB := seedTenant(t, pool, "audit-site-filter-tenant-b")
	sharedSiteID := uuid.New() // simulated collision: same site_id value in both tenants' metadata

	entryA, err := rec.Record(ctx, audit.Event{
		TenantID:   tenantA,
		ActorType:  audit.ActorSystem,
		ActorID:    "",
		Action:     "backup.snapshot.completed",
		TargetType: "backup_snapshot",
		TargetID:   uuid.New().String(),
		Metadata:   map[string]any{"site_id": sharedSiteID.String()},
	})
	if err != nil {
		t.Fatalf("record tenant A event: %v", err)
	}
	entryB, err := rec.Record(ctx, audit.Event{
		TenantID:   tenantB,
		ActorType:  audit.ActorSystem,
		ActorID:    "",
		Action:     "backup.snapshot.completed",
		TargetType: "backup_snapshot",
		TargetID:   uuid.New().String(),
		Metadata:   map[string]any{"site_id": sharedSiteID.String()},
	})
	if err != nil {
		t.Fatalf("record tenant B event: %v", err)
	}

	gotA, err := rec.ListFiltered(ctx, tenantA, audit.Filter{SiteID: &sharedSiteID}, 100, 0)
	if err != nil {
		t.Fatalf("ListFiltered (tenant A) errored: %v", err)
	}
	foundA, foundBInA := false, false
	for _, e := range gotA {
		if e.ID == entryA.ID {
			foundA = true
		}
		if e.ID == entryB.ID {
			foundBInA = true
		}
	}
	if !foundA {
		t.Errorf("tenant A's own matching row must be returned")
	}
	if foundBInA {
		t.Fatalf("SECURITY: tenant B's row leaked into tenant A's site_id-filtered results")
	}

	gotB, err := rec.ListFiltered(ctx, tenantB, audit.Filter{SiteID: &sharedSiteID}, 100, 0)
	if err != nil {
		t.Fatalf("ListFiltered (tenant B) errored: %v", err)
	}
	foundB, foundAInB := false, false
	for _, e := range gotB {
		if e.ID == entryB.ID {
			foundB = true
		}
		if e.ID == entryA.ID {
			foundAInB = true
		}
	}
	if !foundB {
		t.Errorf("tenant B's own matching row must be returned")
	}
	if foundAInB {
		t.Fatalf("SECURITY: tenant A's row leaked into tenant B's site_id-filtered results")
	}
}
