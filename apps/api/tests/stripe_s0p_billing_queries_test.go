package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// These tests prove the two m149 unique indexes and the billing queries that
// sit beside BindCheckoutProvider: FindTenantsByProviderCustomer,
// LockTenantBilling, SetCancelRequested, ListTenantsForReconcile and
// ApplyBillingSubscriptionStateForProvider. Every query runs as wpmgr_app
// through the generated sqlc methods, which is the path the billing service
// uses; tenants carries no row security, so the plain pool (or a transaction
// on it) is the production path.

// s0pWantUnique fails the test unless err is a unique violation raised by the
// named index.
func s0pWantUnique(t *testing.T, err error, index string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("got err=%v, want a 23505 from %s", err, index)
	}
	if pgErr.Code != "23505" || pgErr.ConstraintName != index {
		t.Fatalf("got SQLSTATE %s on %q, want 23505 on %q (%v)", pgErr.Code, pgErr.ConstraintName, index, err)
	}
}

// s0pApply calls ApplyBillingSubscriptionStateForProvider with a plain active
// state; tweak adjusts the parameters.
func s0pApply(ctx context.Context, q *sqlc.Queries, id uuid.UUID, provider, sub, customer string, tweak func(*sqlc.ApplyBillingSubscriptionStateForProviderParams)) (int64, error) {
	p := sqlc.ApplyBillingSubscriptionStateForProviderParams{
		TenantID:               id,
		BillingProvider:        provider,
		Plan:                   string(billing.TierStarter),
		PlanStatus:             "active",
		ProviderSubscriptionID: s01Ptr(sub),
		ProviderCustomerID:     customer,
	}
	if tweak != nil {
		tweak(&p)
	}
	return q.ApplyBillingSubscriptionStateForProvider(ctx, p)
}

type s0pCancel struct {
	PeriodEnd bool
	At        pgtype.Timestamptz
}

func s0pReadCancel(t *testing.T, pool *db.Pool, id uuid.UUID) s0pCancel {
	t.Helper()
	var c s0pCancel
	if err := pool.QueryRow(context.Background(),
		`SELECT cancel_at_period_end, cancel_at FROM tenants WHERE id = $1`, id).
		Scan(&c.PeriodEnd, &c.At); err != nil {
		t.Fatalf("read cancel columns: %v", err)
	}
	return c
}

func TestStripeS0PUniqueIndexesAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	s01AssertAppRole(t, pool)
	q := sqlc.New(pool.Pool)
	u := func() string { return uuid.NewString()[:8] }

	t.Run("binding_tenant_b_to_a_bound_stripe_customer_is_refused", func(t *testing.T) {
		cus := "cus_" + u()
		a := s01Seed(t, pool, s01Pin{})
		if _, err := s01Bind(ctx, q, a, nil, nil, "stripe", &cus); err != nil {
			t.Fatalf("bind A: %v", err)
		}
		b := s01Seed(t, pool, s01Pin{})
		_, err := s01Bind(ctx, q, b, nil, nil, "stripe", &cus)
		s0pWantUnique(t, err, "tenants_stripe_customer_key")
		s01Want(t, s01Read(t, pool, b), "<NULL>", "<NULL>", "<NULL>")
		s01Want(t, s01Read(t, pool, a), "stripe", cus, "<NULL>")
	})

	t.Run("same_provider_fill_with_a_bound_stripe_customer_is_refused", func(t *testing.T) {
		cus := "cus_" + u()
		a := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: &cus, Status: "canceled"})
		// B is pinned to Stripe with no customer, so the bind takes the
		// same-provider branch and tries to fill the NULL customer.
		b := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe")})
		_, err := s01Bind(ctx, q, b, s01Ptr("stripe"), nil, "stripe", &cus)
		s0pWantUnique(t, err, "tenants_stripe_customer_key")
		s01Want(t, s01Read(t, pool, b), "stripe", "<NULL>", "<NULL>")
		s01Want(t, s01Read(t, pool, a), "stripe", cus, "<NULL>")
	})

	t.Run("a_second_tenant_storing_the_same_subscription_id_is_refused", func(t *testing.T) {
		sub := "sub_" + u()
		a := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: s01Ptr("cus_" + u())})
		if n, err := s0pApply(ctx, q, a, "stripe", sub, "", nil); err != nil || n != 1 {
			t.Fatalf("apply A: rows=%d err=%v, want 1 row", n, err)
		}
		b := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: s01Ptr("cus_" + u())})
		_, err := s0pApply(ctx, q, b, "stripe", sub, "", nil)
		s0pWantUnique(t, err, "tenants_provider_subscription_key")
		if got := s01Read(t, pool, b); got.Subscription != nil || got.Status != "none" {
			t.Fatalf("B after the refused apply: subscription=%s status=%s, want <NULL> none", s01Str(got.Subscription), got.Status)
		}
	})

	// The honest cases the indexes must not block.

	t.Run("razorpay_tenants_may_share_a_customer_id", func(t *testing.T) {
		cust := "cust_" + u()
		a := s01Seed(t, pool, s01Pin{})
		b := s01Seed(t, pool, s01Pin{})
		for _, id := range []uuid.UUID{a, b} {
			if _, err := s01Bind(ctx, q, id, nil, nil, "razorpay", &cust); err != nil {
				t.Fatalf("bind %s to a shared Razorpay customer: %v", id, err)
			}
		}
		s01Want(t, s01Read(t, pool, b), "razorpay", cust, "<NULL>")
	})

	t.Run("the_same_subscription_id_under_two_providers_is_allowed", func(t *testing.T) {
		sub := "sub_" + u()
		a := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: s01Ptr("cus_" + u())})
		b := s01Seed(t, pool, s01Pin{Provider: s01Ptr("razorpay")})
		if n, err := s0pApply(ctx, q, a, "stripe", sub, "", nil); err != nil || n != 1 {
			t.Fatalf("apply A: rows=%d err=%v", n, err)
		}
		if n, err := s0pApply(ctx, q, b, "razorpay", sub, "", nil); err != nil || n != 1 {
			t.Fatalf("apply B: rows=%d err=%v", n, err)
		}
	})

	t.Run("a_released_stripe_customer_can_be_bound_by_another_tenant", func(t *testing.T) {
		cus := "cus_" + u()
		a := s01Seed(t, pool, s01Pin{})
		if _, err := s01Bind(ctx, q, a, nil, nil, "stripe", &cus); err != nil {
			t.Fatalf("bind A: %v", err)
		}
		if n, err := s01Clear(ctx, q, a, s01Ptr("stripe"), &cus); err != nil || n != 1 {
			t.Fatalf("clear A: rows=%d err=%v", n, err)
		}
		b := s01Seed(t, pool, s01Pin{})
		if _, err := s01Bind(ctx, q, b, nil, nil, "stripe", &cus); err != nil {
			t.Fatalf("bind B after A released the customer: %v", err)
		}
	})
}

func TestStripeS0PFindTenantsByProviderCustomerAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	s01AssertAppRole(t, pool)
	q := sqlc.New(pool.Pool)
	cust := "cust_" + uuid.NewString()[:8]

	a := s01Seed(t, pool, s01Pin{Provider: s01Ptr("razorpay"), Customer: &cust})
	b := s01Seed(t, pool, s01Pin{Provider: s01Ptr("razorpay"), Customer: &cust})
	// Same customer string under another provider: never returned.
	s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: &cust})
	// A soft-deleted workspace still resolves.
	if _, err := pool.Exec(ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, b); err != nil {
		t.Fatalf("soft-delete B: %v", err)
	}

	got, err := q.FindTenantsByProviderCustomer(ctx, sqlc.FindTenantsByProviderCustomerParams{
		BillingProvider: "razorpay", ProviderCustomerID: cust,
	})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	want := []uuid.UUID{a, b}
	if want[0].String() > want[1].String() {
		want[0], want[1] = want[1], want[0]
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want both Razorpay tenants in id order %v", got, want)
	}

	none, err := q.FindTenantsByProviderCustomer(ctx, sqlc.FindTenantsByProviderCustomerParams{
		BillingProvider: "razorpay", ProviderCustomerID: "cust_nobody_" + uuid.NewString()[:8],
	})
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown customer: got %v err=%v, want an empty result and no error", none, err)
	}
}

func TestStripeS0PApplyForProviderAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	s01AssertAppRole(t, pool)
	q := sqlc.New(pool.Pool)
	u := func() string { return uuid.NewString()[:8] }

	t.Run("writes_state_and_the_cancel_schedule", func(t *testing.T) {
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: s01Ptr("cus_" + u())})
		at := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Microsecond)
		sub := "sub_" + u()
		n, err := s0pApply(ctx, q, id, "stripe", sub, "", func(p *sqlc.ApplyBillingSubscriptionStateForProviderParams) {
			p.CancelAtPeriodEnd = true
			p.CancelAt = pgtype.Timestamptz{Time: at, Valid: true}
		})
		if err != nil || n != 1 {
			t.Fatalf("apply: rows=%d err=%v, want 1 row", n, err)
		}
		c := s0pReadCancel(t, pool, id)
		if !c.PeriodEnd || !c.At.Valid || !c.At.Time.Equal(at) {
			t.Fatalf("cancel columns (%v, %v), want (true, %v)", c.PeriodEnd, c.At.Time, at)
		}
		got := s01Read(t, pool, id)
		if got.Status != "active" || s01Str(got.Subscription) != sub {
			t.Fatalf("state (%s, %s), want (active, %s)", got.Status, s01Str(got.Subscription), sub)
		}

		// A later apply without a schedule clears both.
		if n, err := s0pApply(ctx, q, id, "stripe", sub, "", nil); err != nil || n != 1 {
			t.Fatalf("second apply: rows=%d err=%v", n, err)
		}
		if c := s0pReadCancel(t, pool, id); c.PeriodEnd || c.At.Valid {
			t.Fatalf("cancel columns after a clearing apply (%v, %v), want (false, NULL)", c.PeriodEnd, c.At)
		}
	})

	t.Run("a_moved_pin_writes_nothing", func(t *testing.T) {
		cust := "cust_" + u()
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("razorpay"), Customer: &cust})
		n, err := s0pApply(ctx, q, id, "stripe", "sub_"+u(), "", nil)
		if err != nil || n != 0 {
			t.Fatalf("apply for the wrong provider: rows=%d err=%v, want 0 rows", n, err)
		}
		got := s01Read(t, pool, id)
		s01Want(t, got, "razorpay", cust, "<NULL>")
		if got.Status != "none" {
			t.Fatalf("status %s, want none", got.Status)
		}
		// No pin at all: nothing written either.
		bare := s01Seed(t, pool, s01Pin{})
		if n, err := s0pApply(ctx, q, bare, "stripe", "sub_"+u(), "", nil); err != nil || n != 0 {
			t.Fatalf("apply on an unpinned tenant: rows=%d err=%v, want 0 rows", n, err)
		}
	})

	t.Run("a_stored_customer_id_is_never_replaced", func(t *testing.T) {
		cus := "cus_" + u()
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: &cus})
		sub := "sub_" + u()
		if n, err := s0pApply(ctx, q, id, "stripe", sub, "cus_other_"+u(), nil); err != nil || n != 1 {
			t.Fatalf("apply: rows=%d err=%v", n, err)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cus, sub)
	})

	t.Run("a_null_customer_is_filled_and_an_empty_one_is_ignored", func(t *testing.T) {
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("razorpay")})
		sub := "sub_" + u()
		if n, err := s0pApply(ctx, q, id, "razorpay", sub, "", nil); err != nil || n != 1 {
			t.Fatalf("apply with no customer: rows=%d err=%v", n, err)
		}
		s01Want(t, s01Read(t, pool, id), "razorpay", "<NULL>", sub)
		cust := "cust_" + u()
		if n, err := s0pApply(ctx, q, id, "razorpay", sub, cust, nil); err != nil || n != 1 {
			t.Fatalf("apply with a customer: rows=%d err=%v", n, err)
		}
		s01Want(t, s01Read(t, pool, id), "razorpay", cust, sub)
	})
}

func TestStripeS0PSetCancelRequestedAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	s01AssertAppRole(t, pool)
	q := sqlc.New(pool.Pool)
	u := func() string { return uuid.NewString()[:8] }

	seed := func(provider, status string) (uuid.UUID, string) {
		sub := "sub_" + u()
		return s01Seed(t, pool, s01Pin{Provider: s01Ptr(provider), Customer: s01Ptr("cus_" + u()), Subscription: &sub, Status: status}), sub
	}
	set := func(id uuid.UUID, sub string, now bool, at *time.Time) int64 {
		t.Helper()
		p := sqlc.SetCancelRequestedParams{TenantID: id, ProviderSubscriptionID: sub, CancelNow: now}
		if at != nil {
			p.CancelAt = pgtype.Timestamptz{Time: *at, Valid: true}
		}
		n, err := q.SetCancelRequested(ctx, p)
		if err != nil {
			t.Fatalf("SetCancelRequested: %v", err)
		}
		return n
	}
	untouched := func(id uuid.UUID) {
		t.Helper()
		if c := s0pReadCancel(t, pool, id); c.PeriodEnd || c.At.Valid {
			t.Fatalf("cancel columns (%v, %v), want (false, NULL): a refused write changed the row", c.PeriodEnd, c.At)
		}
	}

	t.Run("cancel_now_marks_a_past_due_stripe_tenant", func(t *testing.T) {
		id, sub := seed("stripe", "past_due")
		if n := set(id, sub, true, nil); n != 1 {
			t.Fatalf("rows=%d, want 1", n)
		}
		c := s0pReadCancel(t, pool, id)
		var dbNow time.Time
		if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
			t.Fatalf("read now: %v", err)
		}
		if !c.PeriodEnd || !c.At.Valid || c.At.Time.After(dbNow) {
			t.Fatalf("cancel columns (%v, %v), want (true, a time at or before %v)", c.PeriodEnd, c.At, dbNow)
		}
	})

	t.Run("cancel_now_refuses_anything_but_stripe_past_due", func(t *testing.T) {
		for _, tc := range []struct{ provider, status string }{
			{"stripe", "active"}, {"stripe", "canceled"}, {"razorpay", "past_due"},
		} {
			id, sub := seed(tc.provider, tc.status)
			if n := set(id, sub, true, nil); n != 0 {
				t.Fatalf("%s %s: rows=%d, want 0", tc.provider, tc.status, n)
			}
			untouched(id)
		}
	})

	t.Run("cancel_now_refuses_a_different_subscription", func(t *testing.T) {
		id, _ := seed("stripe", "past_due")
		if n := set(id, "sub_other_"+u(), true, nil); n != 0 {
			t.Fatalf("rows=%d, want 0", n)
		}
		untouched(id)
	})

	t.Run("local_flag_stores_a_future_cancel_at", func(t *testing.T) {
		id, sub := seed("razorpay", "active")
		at := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Microsecond)
		if n := set(id, sub, false, &at); n != 1 {
			t.Fatalf("rows=%d, want 1", n)
		}
		if c := s0pReadCancel(t, pool, id); !c.PeriodEnd || !c.At.Valid || !c.At.Time.Equal(at) {
			t.Fatalf("cancel columns (%v, %v), want (true, %v)", c.PeriodEnd, c.At, at)
		}
	})

	t.Run("local_flag_never_stores_a_past_cancel_at", func(t *testing.T) {
		for _, ago := range []time.Duration{time.Hour, time.Second} {
			id, sub := seed("razorpay", "past_due")
			at := time.Now().Add(-ago)
			if n := set(id, sub, false, &at); n != 1 {
				t.Fatalf("rows=%d, want 1", n)
			}
			if c := s0pReadCancel(t, pool, id); !c.PeriodEnd || c.At.Valid {
				t.Fatalf("%v ago: cancel columns (%v, %v), want (true, NULL)", ago, c.PeriodEnd, c.At)
			}
		}
		// No period end known: the flag alone.
		id, sub := seed("razorpay", "active")
		if n := set(id, sub, false, nil); n != 1 {
			t.Fatalf("rows=%d, want 1", n)
		}
		if c := s0pReadCancel(t, pool, id); !c.PeriodEnd || c.At.Valid {
			t.Fatalf("cancel columns (%v, %v), want (true, NULL)", c.PeriodEnd, c.At)
		}
	})

	t.Run("local_flag_refuses_stripe_and_ended_or_comped_tenants", func(t *testing.T) {
		at := time.Now().Add(24 * time.Hour)
		for _, tc := range []struct{ provider, status string }{
			{"stripe", "active"}, {"razorpay", "canceled"}, {"razorpay", "comped"}, {"razorpay", "none"},
		} {
			id, sub := seed(tc.provider, tc.status)
			if n := set(id, sub, false, &at); n != 0 {
				t.Fatalf("%s %s: rows=%d, want 0", tc.provider, tc.status, n)
			}
			untouched(id)
		}
	})

	t.Run("local_flag_refuses_a_different_subscription", func(t *testing.T) {
		id, _ := seed("razorpay", "active")
		at := time.Now().Add(24 * time.Hour)
		if n := set(id, "sub_other_"+u(), false, &at); n != 0 {
			t.Fatalf("rows=%d, want 0", n)
		}
		untouched(id)
	})
}

func TestStripeS0PListTenantsForReconcileAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	s01AssertAppRole(t, pool)
	q := sqlc.New(pool.Pool)
	u := func() string { return uuid.NewString()[:8] }

	withSub := s01Seed(t, pool, s01Pin{Provider: s01Ptr("razorpay"), Subscription: s01Ptr("sub_" + u()), Status: "active"})
	stripeCustOnly := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: s01Ptr("cus_" + u()), Status: "canceled"})
	deletedWithSub := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: s01Ptr("cus_" + u()), Subscription: s01Ptr("sub_" + u()), Status: "past_due"})
	if _, err := pool.Exec(ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, deletedWithSub); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	excluded := map[uuid.UUID]string{
		s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: s01Ptr("cus_" + u()), Subscription: s01Ptr("sub_" + u()), Status: "comped"}): "comped",
		s01Seed(t, pool, s01Pin{Provider: s01Ptr("razorpay"), Customer: s01Ptr("cust_" + u())}):                                                    "razorpay customer, no subscription",
		s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe")}):                                                                                       "stripe pin, no customer, no subscription",
		s01Seed(t, pool, s01Pin{}): "no pin",
	}

	rows, err := q.ListTenantsForReconcile(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[uuid.UUID]sqlc.ListTenantsForReconcileRow{}
	for i, r := range rows {
		if i > 0 && rows[i-1].ID.String() >= r.ID.String() {
			t.Fatalf("rows not in id order at %d", i)
		}
		seen[r.ID] = r
	}
	for _, id := range []uuid.UUID{withSub, stripeCustOnly, deletedWithSub} {
		if _, ok := seen[id]; !ok {
			t.Fatalf("tenant %s missing from the reconcile set", id)
		}
	}
	if r := seen[stripeCustOnly]; r.ProviderCustomerID == nil || r.ProviderSubscriptionID != nil {
		t.Fatalf("stripe customer-only row: customer=%s subscription=%s", s01Str(r.ProviderCustomerID), s01Str(r.ProviderSubscriptionID))
	}
	for id, why := range excluded {
		if _, ok := seen[id]; ok {
			t.Fatalf("tenant %s (%s) is in the reconcile set", id, why)
		}
	}
}

func TestStripeS0PLockTenantBillingAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	s01AssertAppRole(t, pool)
	id := s01Seed(t, pool, s01Pin{})
	other := s01Seed(t, pool, s01Pin{})

	begin := func() pgx.Tx {
		t.Helper()
		tx, err := pool.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		return tx
	}
	tryDocumentedKey := func(tx pgx.Tx, tenant uuid.UUID) bool {
		t.Helper()
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT pg_try_advisory_xact_lock(hashtext('wpmgr_billing:' || $1))`, tenant.String()).Scan(&ok); err != nil {
			t.Fatalf("try lock: %v", err)
		}
		return ok
	}

	holder := begin()
	if err := sqlc.New(holder).LockTenantBilling(ctx, id); err != nil {
		t.Fatalf("LockTenantBilling: %v", err)
	}

	probe := begin()
	if tryDocumentedKey(probe, id) {
		t.Fatal("the key 'wpmgr_billing:' || tenant id was free while LockTenantBilling held the tenant's lock")
	}
	if !tryDocumentedKey(probe, other) {
		t.Fatal("another tenant's billing lock was not free")
	}

	// The Go helper the billing service takes today waits on the same key.
	if _, err := probe.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	err := billing.LockTenantBilling(ctx, probe, id)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("billing.LockTenantBilling while the query's lock was held: err=%v, want 55P03 lock_not_available", err)
	}

	// Released at the end of the holder's transaction.
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit holder: %v", err)
	}
	after := begin()
	if !tryDocumentedKey(after, id) {
		t.Fatal("the tenant's billing lock was still held after the holder committed")
	}
}
