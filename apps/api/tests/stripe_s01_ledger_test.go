package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// These tests prove InsertBillingEvent's tenant subquery,
// GetBillingEventByProviderEventID and AuditEntryExistsByKey as wpmgr_app,
// inside the transaction helpers production uses: InAgentTx for the
// billing_events ledger, InTenantTx for audit_log.

func TestStripeS01BillingLedgerAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	s01AssertAppRole(t, pool)

	insert := func(provider, eventID string, claim *uuid.UUID) (uuid.UUID, error) {
		var claimPG pgtype.UUID
		if claim != nil {
			claimPG = pgtype.UUID{Bytes: *claim, Valid: true}
		}
		var id uuid.UUID
		err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
			var qerr error
			id, qerr = sqlc.New(tx).InsertBillingEvent(ctx, sqlc.InsertBillingEventParams{
				Provider:        provider,
				ProviderEventID: eventID,
				Kind:            "checkout.session.completed",
				TenantID:        claimPG,
				Payload:         []byte(`{}`),
				OccurredAt:      time.Now().UTC(),
			})
			return qerr
		})
		return id, err
	}
	get := func(provider, eventID string) (sqlc.BillingEvent, error) {
		var row sqlc.BillingEvent
		err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
			var qerr error
			row, qerr = sqlc.New(tx).GetBillingEventByProviderEventID(ctx, sqlc.GetBillingEventByProviderEventIDParams{
				Provider: provider, ProviderEventID: eventID,
			})
			return qerr
		})
		return row, err
	}
	evt := func() string { return "evt_" + uuid.NewString()[:12] }

	t.Run("existing_claim_is_stored", func(t *testing.T) {
		tenant := seedTenant(t, pool, "s01-ledger-"+uuid.NewString()[:8])
		e := evt()
		id, err := insert("stripe", e, &tenant)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		row, err := get("stripe", e)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if row.ID != id || !row.TenantID.Valid || uuid.UUID(row.TenantID.Bytes) != tenant {
			t.Fatalf("row id=%v tenant=%v, want id=%v tenant=%v", row.ID, row.TenantID, id, tenant)
		}
		if row.ProcessedAt.Valid {
			t.Fatalf("processed_at=%v on a fresh row, want NULL", row.ProcessedAt)
		}
	})

	t.Run("claim_for_a_hard_deleted_tenant_is_recorded_with_null_tenant", func(t *testing.T) {
		tenant := seedTenant(t, pool, "s01-gone-"+uuid.NewString()[:8])
		var deleted bool
		if err := pool.QueryRow(ctx, `SELECT admin_delete_empty_tenant($1)`, tenant).Scan(&deleted); err != nil || !deleted {
			t.Fatalf("hard-delete the tenant: deleted=%v err=%v", deleted, err)
		}
		e := evt()
		if _, err := insert("stripe", e, &tenant); err != nil {
			t.Fatalf("insert with a deleted tenant's claim: %v", err)
		}
		row, err := get("stripe", e)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if row.TenantID.Valid {
			t.Fatalf("tenant_id=%v, want NULL for a deleted tenant", uuid.UUID(row.TenantID.Bytes))
		}
	})

	t.Run("claim_for_a_never_created_tenant_is_recorded_with_null_tenant", func(t *testing.T) {
		ghost := uuid.New()
		e := evt()
		if _, err := insert("stripe", e, &ghost); err != nil {
			t.Fatalf("insert with an unknown claim: %v", err)
		}
		row, err := get("stripe", e)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if row.TenantID.Valid {
			t.Fatalf("tenant_id=%v, want NULL", uuid.UUID(row.TenantID.Bytes))
		}
	})

	t.Run("no_claim_is_null_tenant", func(t *testing.T) {
		e := evt()
		if _, err := insert("razorpay", e, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
		row, err := get("razorpay", e)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if row.TenantID.Valid {
			t.Fatalf("tenant_id=%v, want NULL", uuid.UUID(row.TenantID.Bytes))
		}
	})

	t.Run("duplicate_is_no_rows", func(t *testing.T) {
		e := evt()
		if _, err := insert("stripe", e, nil); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if _, err := insert("stripe", e, nil); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("duplicate insert: err=%v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("get_matches_provider_and_event_id_only", func(t *testing.T) {
		e := evt()
		if _, err := insert("stripe", e, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := get("razorpay", e); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("get with the other provider: err=%v, want pgx.ErrNoRows", err)
		}
		if _, err := get("stripe", evt()); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("get an unknown event: err=%v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("get_reports_processed_at", func(t *testing.T) {
		e := evt()
		id, err := insert("stripe", e, nil)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
			return sqlc.New(tx).MarkBillingEventProcessed(ctx, id)
		}); err != nil {
			t.Fatalf("mark processed: %v", err)
		}
		row, err := get("stripe", e)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !row.ProcessedAt.Valid {
			t.Fatalf("processed_at is NULL after MarkBillingEventProcessed")
		}
	})

	t.Run("get_outside_the_agent_scope_sees_only_own_tenant", func(t *testing.T) {
		a := seedTenant(t, pool, "s01-rls-a-"+uuid.NewString()[:8])
		b := seedTenant(t, pool, "s01-rls-b-"+uuid.NewString()[:8])
		e := evt()
		if _, err := insert("stripe", e, &a); err != nil {
			t.Fatalf("insert: %v", err)
		}
		read := func(tenant uuid.UUID) error {
			return pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
				_, err := sqlc.New(tx).GetBillingEventByProviderEventID(ctx, sqlc.GetBillingEventByProviderEventIDParams{
					Provider: "stripe", ProviderEventID: e,
				})
				return err
			})
		}
		if err := read(a); err != nil {
			t.Fatalf("owning tenant's read: %v", err)
		}
		if err := read(b); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("other tenant's read: err=%v, want pgx.ErrNoRows", err)
		}
	})
}

func TestStripeS01AuditEntryExistsByKeyAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	s01AssertAppRole(t, pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	a := seedTenant(t, pool, "s01-audit-a-"+uuid.NewString()[:8])
	b := seedTenant(t, pool, "s01-audit-b-"+uuid.NewString()[:8])

	exists := func(tenant uuid.UUID, key string) bool {
		t.Helper()
		var ok bool
		if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			var qerr error
			ok, qerr = sqlc.New(tx).AuditEntryExistsByKey(ctx, sqlc.AuditEntryExistsByKeyParams{TenantID: tenant, AuditKey: key})
			return qerr
		}); err != nil {
			t.Fatalf("AuditEntryExistsByKey(%v, %q): %v", tenant, key, err)
		}
		return ok
	}
	record := func(tenant uuid.UUID, meta map[string]any) {
		t.Helper()
		if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			_, err := rec.RecordInTx(ctx, tx, audit.Event{
				TenantID: tenant, Action: "billing.s01_test", TargetType: "tenant", TargetID: tenant.String(), Metadata: meta,
			})
			return err
		}); err != nil {
			t.Fatalf("record audit entry: %v", err)
		}
	}

	k1, k2 := "evt_"+uuid.NewString()[:12], "evt_"+uuid.NewString()[:12]
	if exists(a, k1) {
		t.Fatalf("key reported present before any entry was recorded")
	}
	record(a, map[string]any{"audit_key": k1})
	record(a, map[string]any{"note": k2})
	if !exists(a, k1) {
		t.Fatalf("recorded key %q not found for its tenant", k1)
	}
	if exists(a, k2) {
		t.Fatalf("key %q found, but only appears as another metadata field", k2)
	}
	if exists(a, "evt_never") {
		t.Fatalf("an unrecorded key was found")
	}
	if exists(b, k1) {
		t.Fatalf("tenant A's key %q found for tenant B", k1)
	}
}
