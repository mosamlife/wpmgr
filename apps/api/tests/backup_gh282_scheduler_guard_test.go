// backup_gh282_scheduler_guard_test.go: GH #282 regression, archiving a site
// did not stop its backup schedule (scheduled backups kept firing/failing/
// emailing for an unmanaged site).
//
// These run against a REAL Postgres (testcontainers) so the actual guarded
// SQL in internal/backup/repo.go (ListDueSchedules, ClaimAndAdvanceDueSchedules,
// HealOverdueSchedules, all three now JOIN sites, and skip a site whose
// connection_state is archived/revoked or whose tenant is soft-deleted) is
// exercised end to end, under the real RLS policies, connected as the
// non-superuser application role. The narrower in-memory-fake coverage for
// CreateBackup's manual-run guard and sendBackupEmail's suppression lives in
// internal/backup/gh282_site_state_guard_test.go.
package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// seedGuardSite inserts a site row with the given connection_state and
// returns its id. connection_state defaults to 'pending_enrollment' when
// state == "" (the column's own schema default).
func seedGuardSite(t *testing.T, admin *db.Pool, tenant uuid.UUID, state string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	var err error
	if state == "" {
		err = admin.QueryRow(context.Background(),
			`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'seed') RETURNING id`,
			tenant, "https://"+uuid.NewString()+".example.com").Scan(&id)
	} else {
		err = admin.QueryRow(context.Background(),
			`INSERT INTO sites (tenant_id, url, name, connection_state) VALUES ($1, $2, 'seed', $3) RETURNING id`,
			tenant, "https://"+uuid.NewString()+".example.com", state).Scan(&id)
	}
	if err != nil {
		t.Fatalf("seed site (state=%q): %v", state, err)
	}
	return id
}

// seedGuardSchedule inserts a backup_schedules row due at nextRunAt and
// returns its id. Every other column takes its schema default (enabled=true,
// daily cadence, etc.); the guard predicate only cares about enabled,
// next_run_at, and the joined site/tenant state.
func seedGuardSchedule(t *testing.T, admin *db.Pool, tenant, site uuid.UUID, nextRunAt time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := admin.QueryRow(context.Background(),
		`INSERT INTO backup_schedules (tenant_id, site_id, next_run_at) VALUES ($1, $2, $3) RETURNING id`,
		tenant, site, nextRunAt).Scan(&id)
	if err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	return id
}

// readScheduleNextRunAt reads the current next_run_at for a schedule id,
// truncated to microsecond precision (Postgres timestamptz truncation; see
// the sibling uptime-rollup precision fix) so it compares safely against a
// Go time.Time built at nanosecond precision.
func readScheduleNextRunAt(t *testing.T, admin *db.Pool, id uuid.UUID) time.Time {
	t.Helper()
	var next time.Time
	if err := admin.QueryRow(context.Background(),
		`SELECT next_run_at FROM backup_schedules WHERE id = $1`, id).Scan(&next); err != nil {
		t.Fatalf("read schedule next_run_at: %v", err)
	}
	return next.Truncate(time.Microsecond)
}

func setGuardSiteConnectionState(t *testing.T, admin *db.Pool, site uuid.UUID, state string) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`UPDATE sites SET connection_state = $2 WHERE id = $1`, site, state); err != nil {
		t.Fatalf("update site connection_state: %v", err)
	}
}

func softDeleteGuardTenant(t *testing.T, admin *db.Pool, tenant uuid.UUID) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`UPDATE tenants SET deleted_at = now() WHERE id = $1`, tenant); err != nil {
		t.Fatalf("soft-delete tenant: %v", err)
	}
}

func idSet(schedules []backup.Schedule) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool, len(schedules))
	for _, s := range schedules {
		out[s.ID] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// ClaimAndAdvanceDueSchedules: the live atomic claim path.
// ---------------------------------------------------------------------------

// TestBackupSchedulerGuard_ClaimAndAdvance_SkipsArchivedAndRevoked_KeepsOtherStates
// seeds one due schedule per connection_state and proves: archived and
// revoked are neither claimed (returned) nor advanced (next_run_at
// unchanged); connected, degraded, disconnected, and pending_enrollment are
// both claimed and advanced. This is the primary GH #282 fix surface, the
// scheduler's actual firing path.
func TestBackupSchedulerGuard_ClaimAndAdvance_SkipsArchivedAndRevoked_KeepsOtherStates(t *testing.T) {
	app := startPostgres(t)
	admin := connectAdmin(t, app)
	ctx := context.Background()

	tenant := seedTenant(t, admin, "gh282-claim-"+uuid.NewString())

	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(-1 * time.Hour)

	states := map[string]string{
		"connected":          "connected",
		"degraded":           "degraded",
		"disconnected":       "disconnected",
		"pending_enrollment": "", // "" seeds the schema default, pending_enrollment
		"archived":           "archived",
		"revoked":            "revoked",
	}
	scheduleIDs := make(map[string]uuid.UUID, len(states))
	for label, state := range states {
		site := seedGuardSite(t, admin, tenant, state)
		scheduleIDs[label] = seedGuardSchedule(t, admin, tenant, site, due)
	}

	repo := backup.NewRepo(app)

	nextAt := make(map[uuid.UUID]time.Time, len(scheduleIDs))
	advanceTo := now.Add(24 * time.Hour)
	for _, id := range scheduleIDs {
		nextAt[id] = advanceTo
	}

	claimed, err := repo.ClaimAndAdvanceDueSchedules(ctx, now, nextAt)
	if err != nil {
		t.Fatalf("ClaimAndAdvanceDueSchedules: %v", err)
	}
	claimedIDs := idSet(claimed)

	keepFiring := []string{"connected", "degraded", "disconnected", "pending_enrollment"}
	skip := []string{"archived", "revoked"}

	for _, label := range keepFiring {
		id := scheduleIDs[label]
		if !claimedIDs[id] {
			t.Errorf("%s site: schedule was NOT claimed; expected it to keep firing", label)
		}
		if got := readScheduleNextRunAt(t, admin, id); !got.Equal(advanceTo) {
			t.Errorf("%s site: next_run_at = %v, want advanced to %v", label, got, advanceTo)
		}
	}
	for _, label := range skip {
		id := scheduleIDs[label]
		if claimedIDs[id] {
			t.Errorf("%s site: schedule WAS claimed; expected the scheduler to skip a deliberately unmanaged site", label)
		}
		if got := readScheduleNextRunAt(t, admin, id); !got.Equal(due) {
			t.Errorf("%s site: next_run_at = %v, want UNCHANGED at %v (no advance, no enqueue)", label, got, due)
		}
	}
}

// TestBackupSchedulerGuard_ClaimAndAdvance_SkipsSoftDeletedTenant proves a
// schedule belonging to a soft-deleted tenant (m93) is skipped even though
// its site is connected: the latent sibling gap on the same query surface.
func TestBackupSchedulerGuard_ClaimAndAdvance_SkipsSoftDeletedTenant(t *testing.T) {
	app := startPostgres(t)
	admin := connectAdmin(t, app)
	ctx := context.Background()

	tenant := seedTenant(t, admin, "gh282-softdel-"+uuid.NewString())
	site := seedGuardSite(t, admin, tenant, "connected")

	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(-1 * time.Hour)
	schedID := seedGuardSchedule(t, admin, tenant, site, due)

	softDeleteGuardTenant(t, admin, tenant)

	repo := backup.NewRepo(app)
	advanceTo := now.Add(24 * time.Hour)
	claimed, err := repo.ClaimAndAdvanceDueSchedules(ctx, now, map[uuid.UUID]time.Time{schedID: advanceTo})
	if err != nil {
		t.Fatalf("ClaimAndAdvanceDueSchedules: %v", err)
	}
	if idSet(claimed)[schedID] {
		t.Error("soft-deleted tenant: schedule WAS claimed; expected it to be skipped")
	}
	if got := readScheduleNextRunAt(t, admin, schedID); !got.Equal(due) {
		t.Errorf("soft-deleted tenant: next_run_at = %v, want UNCHANGED at %v", got, due)
	}
}

// TestBackupSchedulerGuard_RestoredSite_BecomesEligibleAgain proves the
// guard is non-destructive: once an archived site is restored (connection_state
// leaves 'archived'), its still-due schedule becomes claimable again on the
// very next scheduler tick, with no manual re-enable step.
func TestBackupSchedulerGuard_RestoredSite_BecomesEligibleAgain(t *testing.T) {
	app := startPostgres(t)
	admin := connectAdmin(t, app)
	ctx := context.Background()

	tenant := seedTenant(t, admin, "gh282-restore-"+uuid.NewString())
	site := seedGuardSite(t, admin, tenant, "archived")

	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(-1 * time.Hour)
	schedID := seedGuardSchedule(t, admin, tenant, site, due)

	repo := backup.NewRepo(app)
	advanceTo := now.Add(24 * time.Hour)

	// First tick: still archived, must be skipped.
	claimed, err := repo.ClaimAndAdvanceDueSchedules(ctx, now, map[uuid.UUID]time.Time{schedID: advanceTo})
	if err != nil {
		t.Fatalf("ClaimAndAdvanceDueSchedules (archived): %v", err)
	}
	if idSet(claimed)[schedID] {
		t.Fatal("archived site: schedule was claimed on the first tick; expected it to be skipped")
	}
	if got := readScheduleNextRunAt(t, admin, schedID); !got.Equal(due) {
		t.Fatalf("archived site: next_run_at advanced unexpectedly to %v", got)
	}

	// Restore: connection_state leaves 'archived' (e.g. site.Restore() moves it
	// to 'disconnected' pending a fresh heartbeat).
	setGuardSiteConnectionState(t, admin, site, "disconnected")

	// Second tick: the schedule is still due (never advanced) and the site is
	// no longer archived, so it must now be claimed and advanced. This is one
	// immediate catch-up run, which is the accepted/intended behaviour.
	claimed, err = repo.ClaimAndAdvanceDueSchedules(ctx, now, map[uuid.UUID]time.Time{schedID: advanceTo})
	if err != nil {
		t.Fatalf("ClaimAndAdvanceDueSchedules (restored): %v", err)
	}
	if !idSet(claimed)[schedID] {
		t.Fatal("restored site: schedule was NOT claimed; expected it to become eligible again")
	}
	if got := readScheduleNextRunAt(t, admin, schedID); !got.Equal(advanceTo) {
		t.Errorf("restored site: next_run_at = %v, want advanced to %v", got, advanceTo)
	}
}

// ---------------------------------------------------------------------------
// ListDueSchedules: the candidate-enumeration path (ClaimDueSchedules'
// pre-computation pass reads this before the atomic claim).
// ---------------------------------------------------------------------------

func TestBackupSchedulerGuard_ListDueSchedules_ExcludesArchivedAndRevoked(t *testing.T) {
	app := startPostgres(t)
	admin := connectAdmin(t, app)
	ctx := context.Background()

	tenant := seedTenant(t, admin, "gh282-list-"+uuid.NewString())
	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(-1 * time.Hour)

	connectedSite := seedGuardSite(t, admin, tenant, "connected")
	archivedSite := seedGuardSite(t, admin, tenant, "archived")
	revokedSite := seedGuardSite(t, admin, tenant, "revoked")

	connectedSchedID := seedGuardSchedule(t, admin, tenant, connectedSite, due)
	archivedSchedID := seedGuardSchedule(t, admin, tenant, archivedSite, due)
	revokedSchedID := seedGuardSchedule(t, admin, tenant, revokedSite, due)

	repo := backup.NewRepo(app)
	due2, err := repo.ListDueSchedules(ctx, now, 200)
	if err != nil {
		t.Fatalf("ListDueSchedules: %v", err)
	}
	ids := idSet(due2)
	if !ids[connectedSchedID] {
		t.Error("connected site's schedule missing from ListDueSchedules")
	}
	if ids[archivedSchedID] {
		t.Error("archived site's schedule present in ListDueSchedules; expected it excluded")
	}
	if ids[revokedSchedID] {
		t.Error("revoked site's schedule present in ListDueSchedules; expected it excluded")
	}
}

// ---------------------------------------------------------------------------
// HealOverdueSchedules: the boot-time heal pass.
// ---------------------------------------------------------------------------

func TestBackupSchedulerGuard_HealOverdueSchedules_SkipsArchivedRevokedAndSoftDeletedTenant(t *testing.T) {
	app := startPostgres(t)
	admin := connectAdmin(t, app)
	ctx := context.Background()

	activeTenant := seedTenant(t, admin, "gh282-heal-active-"+uuid.NewString())
	deletedTenant := seedTenant(t, admin, "gh282-heal-deleted-"+uuid.NewString())
	softDeleteGuardTenant(t, admin, deletedTenant)

	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(-1 * time.Hour)

	connectedSite := seedGuardSite(t, admin, activeTenant, "connected")
	archivedSite := seedGuardSite(t, admin, activeTenant, "archived")
	revokedSite := seedGuardSite(t, admin, activeTenant, "revoked")
	deletedTenantSite := seedGuardSite(t, admin, deletedTenant, "connected")

	connectedSchedID := seedGuardSchedule(t, admin, activeTenant, connectedSite, due)
	archivedSchedID := seedGuardSchedule(t, admin, activeTenant, archivedSite, due)
	revokedSchedID := seedGuardSchedule(t, admin, activeTenant, revokedSite, due)
	deletedTenantSchedID := seedGuardSchedule(t, admin, deletedTenant, deletedTenantSite, due)

	repo := backup.NewRepo(app)
	healedTo := now.Add(48 * time.Hour)
	compute := func(_ backup.Schedule, _ time.Time) time.Time { return healedTo }

	healed, err := repo.HealOverdueSchedules(ctx, now, compute)
	if err != nil {
		t.Fatalf("HealOverdueSchedules: %v", err)
	}
	if healed != 1 {
		t.Errorf("healed = %d, want 1 (only the connected site's schedule)", healed)
	}

	if got := readScheduleNextRunAt(t, admin, connectedSchedID); !got.Equal(healedTo) {
		t.Errorf("connected site: next_run_at = %v, want healed to %v", got, healedTo)
	}
	for label, id := range map[string]uuid.UUID{
		"archived":            archivedSchedID,
		"revoked":             revokedSchedID,
		"soft-deleted tenant": deletedTenantSchedID,
	} {
		if got := readScheduleNextRunAt(t, admin, id); !got.Equal(due) {
			t.Errorf("%s: next_run_at = %v, want UNCHANGED at %v (not healed)", label, got, due)
		}
	}
}
