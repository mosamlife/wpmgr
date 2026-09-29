package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// These tests prove the m149 billing-binding queries, BindCheckoutProvider and
// AdminClearBillingPin, as the wpmgr_app role through the generated sqlc
// methods, the path the billing service uses. tenants carries no row
// security, so the plain pool is the production path for both.

func s01Ptr(s string) *string { return &s }

func s01Str(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

// s01AssertAppRole fails the test unless the pool's connections run as a role
// that row security applies to. Every assertion below is only meaningful as
// the production role.
func s01AssertAppRole(t *testing.T, pool *db.Pool) {
	t.Helper()
	var role string
	var super, bypass bool
	if err := pool.QueryRow(context.Background(),
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&role, &super, &bypass); err != nil {
		t.Fatalf("read current role: %v", err)
	}
	if role != "wpmgr_app" || super || bypass {
		t.Fatalf("connected as %q rolsuper=%v rolbypassrls=%v; these proofs must run as wpmgr_app", role, super, bypass)
	}
}

type s01Pin struct {
	Provider, Customer, Subscription *string
	Status                           string
}

// s01Seed creates a tenant and sets its billing columns directly: a fixture,
// not the path under test.
func s01Seed(t *testing.T, pool *db.Pool, pin s01Pin) uuid.UUID {
	t.Helper()
	id := seedTenant(t, pool, "s01-"+uuid.NewString()[:12])
	s01Set(t, pool, id, pin)
	return id
}

func s01Set(t *testing.T, pool *db.Pool, id uuid.UUID, pin s01Pin) {
	t.Helper()
	status := pin.Status
	if status == "" {
		status = "none"
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE tenants SET billing_provider = $2, provider_customer_id = $3,
		        provider_subscription_id = $4, plan_status = $5 WHERE id = $1`,
		id, pin.Provider, pin.Customer, pin.Subscription, status); err != nil {
		t.Fatalf("seed billing columns: %v", err)
	}
}

func s01Read(t *testing.T, pool *db.Pool, id uuid.UUID) s01Pin {
	t.Helper()
	var p s01Pin
	if err := pool.QueryRow(context.Background(),
		`SELECT billing_provider, provider_customer_id, provider_subscription_id, plan_status
		   FROM tenants WHERE id = $1`, id).
		Scan(&p.Provider, &p.Customer, &p.Subscription, &p.Status); err != nil {
		t.Fatalf("read billing columns: %v", err)
	}
	return p
}

func s01Want(t *testing.T, got s01Pin, provider, customer, sub string) {
	t.Helper()
	if s01Str(got.Provider) != provider || s01Str(got.Customer) != customer || s01Str(got.Subscription) != sub {
		t.Fatalf("row is (%s, %s, %s), want (%s, %s, %s)",
			s01Str(got.Provider), s01Str(got.Customer), s01Str(got.Subscription), provider, customer, sub)
	}
}

func s01Bind(ctx context.Context, q *sqlc.Queries, id uuid.UUID, expProvider, expCustomer *string, newProvider string, customer *string) (*string, error) {
	return q.BindCheckoutProvider(ctx, sqlc.BindCheckoutProviderParams{
		TenantID:         id,
		ExpectedProvider: expProvider,
		ExpectedCustomer: expCustomer,
		NewProvider:      newProvider,
		CustomerID:       customer,
	})
}

func s01Clear(ctx context.Context, q *sqlc.Queries, id uuid.UUID, expProvider, expCustomer *string) (int64, error) {
	return q.AdminClearBillingPin(ctx, sqlc.AdminClearBillingPinParams{
		TenantID:         id,
		ExpectedProvider: expProvider,
		ExpectedCustomer: expCustomer,
	})
}

func TestStripeS01BindCheckoutProviderAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	s01AssertAppRole(t, pool)
	q := sqlc.New(pool.Pool)
	u := func() string { return uuid.NewString()[:8] }

	t.Run("first_pin_from_null_binds_and_returns_the_customer", func(t *testing.T) {
		id := s01Seed(t, pool, s01Pin{})
		cus := "cus_" + u()
		got, err := s01Bind(ctx, q, id, nil, nil, "stripe", &cus)
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		if s01Str(got) != cus {
			t.Fatalf("returned customer %s, want %s", s01Str(got), cus)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cus, "<NULL>")
	})

	t.Run("second_same_provider_bind_keeps_the_first_customer", func(t *testing.T) {
		// A double click on the first checkout: both requests read a NULL pin.
		id := s01Seed(t, pool, s01Pin{})
		cus1, cus2 := "cus_"+u(), "cus_"+u()
		if _, err := s01Bind(ctx, q, id, nil, nil, "stripe", &cus1); err != nil {
			t.Fatalf("first bind: %v", err)
		}
		got, err := s01Bind(ctx, q, id, nil, nil, "stripe", &cus2)
		if err != nil {
			t.Fatalf("second same-provider bind refused: %v", err)
		}
		if s01Str(got) != cus1 {
			t.Fatalf("second bind returned %s, want the first customer %s", s01Str(got), cus1)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cus1, "<NULL>")
	})

	t.Run("same_provider_keeps_customer_and_subscription", func(t *testing.T) {
		cus, sub := "cus_"+u(), "sub_"+u()
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: &cus, Subscription: &sub, Status: "canceled"})
		other := "cus_" + u()
		got, err := s01Bind(ctx, q, id, s01Ptr("stripe"), &cus, "stripe", &other)
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		if s01Str(got) != cus {
			t.Fatalf("returned %s, want the stored %s", s01Str(got), cus)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cus, sub)
	})

	t.Run("same_provider_fills_a_missing_customer", func(t *testing.T) {
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe")})
		cus := "cus_" + u()
		got, err := s01Bind(ctx, q, id, s01Ptr("stripe"), nil, "stripe", &cus)
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		if s01Str(got) != cus {
			t.Fatalf("returned %s, want %s", s01Str(got), cus)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cus, "<NULL>")
	})

	t.Run("switch_from_the_checked_pair_moves_and_clears", func(t *testing.T) {
		cus, sub := "cus_"+u(), "sub_"+u()
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: &cus, Subscription: &sub, Status: "canceled"})
		got, err := s01Bind(ctx, q, id, s01Ptr("stripe"), &cus, "razorpay", nil)
		if err != nil {
			t.Fatalf("switch: %v", err)
		}
		if got != nil {
			t.Fatalf("switch returned customer %s, want NULL", s01Str(got))
		}
		s01Want(t, s01Read(t, pool, id), "razorpay", "<NULL>", "<NULL>")
	})

	t.Run("refused_when_the_pin_moved_to_another_provider", func(t *testing.T) {
		// Read a NULL pin; another request pinned razorpay before this write.
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("razorpay")})
		cus := "cus_" + u()
		_, err := s01Bind(ctx, q, id, nil, nil, "stripe", &cus)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("bind from a moved pin: err=%v, want pgx.ErrNoRows", err)
		}
		s01Want(t, s01Read(t, pool, id), "razorpay", "<NULL>", "<NULL>")
	})

	t.Run("refused_when_the_customer_changed_under_the_same_pin", func(t *testing.T) {
		// Checked (stripe, cus_A); the row is now (stripe, cus_B).
		cusA, cusB := "cus_"+u(), "cus_"+u()
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: &cusB})
		_, err := s01Bind(ctx, q, id, s01Ptr("stripe"), &cusA, "razorpay", nil)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("switch with a stale expected customer: err=%v, want pgx.ErrNoRows", err)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cusB, "<NULL>")
	})

	t.Run("refused_when_a_null_customer_was_filled", func(t *testing.T) {
		// Checked (stripe, NULL); a same-provider bind has since stored cus_B.
		cusB := "cus_" + u()
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: &cusB})
		_, err := s01Bind(ctx, q, id, s01Ptr("stripe"), nil, "razorpay", nil)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("switch after a customer fill: err=%v, want pgx.ErrNoRows", err)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cusB, "<NULL>")
	})

	t.Run("unknown_tenant_is_zero_rows", func(t *testing.T) {
		_, err := s01Bind(ctx, q, uuid.New(), nil, nil, "stripe", s01Ptr("cus_"+u()))
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("bind on an unknown tenant: err=%v, want pgx.ErrNoRows", err)
		}
	})
}

func TestStripeS01AdminClearBillingPinAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	s01AssertAppRole(t, pool)
	q := sqlc.New(pool.Pool)
	u := func() string { return uuid.NewString()[:8] }

	t.Run("clears_all_three_from_the_checked_pair", func(t *testing.T) {
		cus, sub := "cus_"+u(), "sub_"+u()
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: &cus, Subscription: &sub, Status: "canceled"})
		n, err := s01Clear(ctx, q, id, s01Ptr("stripe"), &cus)
		if err != nil || n != 1 {
			t.Fatalf("clear: n=%d err=%v, want 1 row", n, err)
		}
		s01Want(t, s01Read(t, pool, id), "<NULL>", "<NULL>", "<NULL>")
	})

	t.Run("refused_when_the_provider_moved", func(t *testing.T) {
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("razorpay")})
		n, err := s01Clear(ctx, q, id, s01Ptr("stripe"), nil)
		if err != nil || n != 0 {
			t.Fatalf("clear with a stale expected provider: n=%d err=%v, want 0 rows", n, err)
		}
		s01Want(t, s01Read(t, pool, id), "razorpay", "<NULL>", "<NULL>")
	})

	t.Run("refused_when_the_customer_changed", func(t *testing.T) {
		cusA, cusB := "cus_"+u(), "cus_"+u()
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: &cusB})
		n, err := s01Clear(ctx, q, id, s01Ptr("stripe"), &cusA)
		if err != nil || n != 0 {
			t.Fatalf("clear with a stale expected customer: n=%d err=%v, want 0 rows", n, err)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cusB, "<NULL>")
	})

	t.Run("refused_when_a_null_customer_was_filled", func(t *testing.T) {
		cusB := "cus_" + u()
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: &cusB})
		n, err := s01Clear(ctx, q, id, s01Ptr("stripe"), nil)
		if err != nil || n != 0 {
			t.Fatalf("clear after a customer fill: n=%d err=%v, want 0 rows", n, err)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cusB, "<NULL>")
	})

	t.Run("unknown_tenant_is_zero_rows", func(t *testing.T) {
		n, err := s01Clear(ctx, q, uuid.New(), nil, nil)
		if err != nil || n != 0 {
			t.Fatalf("clear on an unknown tenant: n=%d err=%v, want 0 rows", n, err)
		}
	})
}

// TestStripeS01BindAndClearShareOnePredicateAsAppRole runs the same status
// matrix through a switch, a same-provider bind and a clear. All three must
// accept exactly canceled (with or without a stored subscription) and none
// with no stored subscription, and refuse every other state.
func TestStripeS01BindAndClearShareOnePredicateAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	s01AssertAppRole(t, pool)
	q := sqlc.New(pool.Pool)

	cases := []struct {
		status  string
		withSub bool
		movable bool
	}{
		{"none", false, true},
		{"none", true, false},
		{"canceled", false, true},
		{"canceled", true, true},
		{"active", true, false},
		{"trialing", true, false},
		{"past_due", true, false},
		{"paused", true, false},
		{"comped", false, false},
		{"comped", true, false},
	}
	for _, c := range cases {
		name := c.status
		if c.withSub {
			name += "_with_subscription"
		}
		t.Run(name, func(t *testing.T) {
			mk := func() (uuid.UUID, string) {
				cus := "cus_" + uuid.NewString()[:8]
				pin := s01Pin{Provider: s01Ptr("stripe"), Customer: &cus, Status: c.status}
				if c.withSub {
					pin.Subscription = s01Ptr("sub_" + uuid.NewString()[:8])
				}
				return s01Seed(t, pool, pin), cus
			}

			swID, swCus := mk()
			_, err := s01Bind(ctx, q, swID, s01Ptr("stripe"), &swCus, "razorpay", nil)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("switch: %v", err)
			}
			switched := err == nil

			spID, spCus := mk()
			_, err = s01Bind(ctx, q, spID, s01Ptr("stripe"), &spCus, "stripe", nil)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("same-provider bind: %v", err)
			}
			samebound := err == nil

			clID, clCus := mk()
			n, err := s01Clear(ctx, q, clID, s01Ptr("stripe"), &clCus)
			if err != nil {
				t.Fatalf("clear: %v", err)
			}
			cleared := n == 1

			if switched != c.movable || samebound != c.movable || cleared != c.movable {
				t.Fatalf("status %s withSub=%v: switch=%v same-provider=%v clear=%v, want all %v",
					c.status, c.withSub, switched, samebound, cleared, c.movable)
			}
		})
	}
}

// s01WaitBlocked polls, a bounded number of times, until some backend is
// waiting on a lock while running a statement that contains marker.
func s01WaitBlocked(t *testing.T, pool *db.Pool, marker string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%'
			    AND pid <> pg_backend_pid()`, marker).Scan(&n); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("no backend blocked on a lock in %q after 200 polls; the race was not staged", marker)
}

type s01Result struct {
	customer *string
	rows     int64
	err      error
}

// TestStripeS01CompareAndSetRacesAsAppRole stages each race for real: the
// first writer holds the tenant row, the second is proven blocked on it, the
// first commits, and the second's WHERE is re-evaluated against the committed
// row.
func TestStripeS01CompareAndSetRacesAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	s01AssertAppRole(t, pool)
	u := func() string { return uuid.NewString()[:8] }

	// race runs first() in an open transaction, starts second() in its own
	// transaction, waits until second is blocked, commits first, and returns
	// second's result.
	race := func(t *testing.T, first func(q *sqlc.Queries) error, marker string, second func(q *sqlc.Queries) s01Result) s01Result {
		t.Helper()
		tx1, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin first: %v", err)
		}
		defer func() { _ = tx1.Rollback(ctx) }()
		if err := first(sqlc.New(tx1)); err != nil {
			t.Fatalf("first writer: %v", err)
		}
		out := make(chan s01Result, 1)
		go func() {
			tx2, err := pool.Begin(ctx)
			if err != nil {
				out <- s01Result{err: err}
				return
			}
			r := second(sqlc.New(tx2))
			if r.err == nil {
				r.err = tx2.Commit(ctx)
			} else {
				_ = tx2.Rollback(ctx)
			}
			out <- r
		}()
		s01WaitBlocked(t, pool, marker)
		if err := tx1.Commit(ctx); err != nil {
			t.Fatalf("commit first: %v", err)
		}
		select {
		case r := <-out:
			return r
		case <-time.After(15 * time.Second):
			t.Fatalf("second writer did not finish within 15s of the first commit")
		}
		return s01Result{}
	}

	bind := func(id uuid.UUID, expP, expC *string, newP string, cus *string) func(q *sqlc.Queries) s01Result {
		return func(q *sqlc.Queries) s01Result {
			c, err := s01Bind(ctx, q, id, expP, expC, newP, cus)
			return s01Result{customer: c, err: err}
		}
	}

	t.Run("stripe_then_razorpay_from_null_pin", func(t *testing.T) {
		id := s01Seed(t, pool, s01Pin{})
		cus := "cus_" + u()
		r := race(t, func(q *sqlc.Queries) error {
			_, err := s01Bind(ctx, q, id, nil, nil, "stripe", &cus)
			return err
		}, "BindCheckoutProvider", bind(id, nil, nil, "razorpay", nil))
		if !errors.Is(r.err, pgx.ErrNoRows) {
			t.Fatalf("second (razorpay) bind: err=%v, want pgx.ErrNoRows", r.err)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cus, "<NULL>")
	})

	t.Run("razorpay_then_stripe_from_null_pin", func(t *testing.T) {
		id := s01Seed(t, pool, s01Pin{})
		r := race(t, func(q *sqlc.Queries) error {
			_, err := s01Bind(ctx, q, id, nil, nil, "razorpay", nil)
			return err
		}, "BindCheckoutProvider", bind(id, nil, nil, "stripe", s01Ptr("cus_"+u())))
		if !errors.Is(r.err, pgx.ErrNoRows) {
			t.Fatalf("second (stripe) bind: err=%v, want pgx.ErrNoRows", r.err)
		}
		s01Want(t, s01Read(t, pool, id), "razorpay", "<NULL>", "<NULL>")
	})

	t.Run("two_stripe_binds_from_null_pin_both_succeed_on_one_customer", func(t *testing.T) {
		id := s01Seed(t, pool, s01Pin{})
		cus1, cus2 := "cus_"+u(), "cus_"+u()
		r := race(t, func(q *sqlc.Queries) error {
			_, err := s01Bind(ctx, q, id, nil, nil, "stripe", &cus1)
			return err
		}, "BindCheckoutProvider", bind(id, nil, nil, "stripe", &cus2))
		if r.err != nil {
			t.Fatalf("second same-provider bind: %v", r.err)
		}
		if s01Str(r.customer) != cus1 {
			t.Fatalf("second bind returned %s, want the first customer %s", s01Str(r.customer), cus1)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cus1, "<NULL>")
	})

	t.Run("switch_commits_before_a_stripe_bind_from_the_old_pin", func(t *testing.T) {
		cus := "cus_" + u()
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: &cus})
		r := race(t, func(q *sqlc.Queries) error {
			_, err := s01Bind(ctx, q, id, s01Ptr("stripe"), &cus, "razorpay", nil)
			return err
		}, "BindCheckoutProvider", bind(id, s01Ptr("stripe"), &cus, "stripe", &cus))
		if !errors.Is(r.err, pgx.ErrNoRows) {
			t.Fatalf("stripe bind after the switch: err=%v, want pgx.ErrNoRows", r.err)
		}
		s01Want(t, s01Read(t, pool, id), "razorpay", "<NULL>", "<NULL>")
	})

	t.Run("customer_fill_commits_before_a_switch_from_null_customer", func(t *testing.T) {
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe")})
		cusB := "cus_" + u()
		r := race(t, func(q *sqlc.Queries) error {
			_, err := s01Bind(ctx, q, id, s01Ptr("stripe"), nil, "stripe", &cusB)
			return err
		}, "BindCheckoutProvider", bind(id, s01Ptr("stripe"), nil, "razorpay", nil))
		if !errors.Is(r.err, pgx.ErrNoRows) {
			t.Fatalf("switch after the fill: err=%v, want pgx.ErrNoRows", r.err)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cusB, "<NULL>")
	})

	t.Run("clear_after_a_switch_commits_is_zero_rows", func(t *testing.T) {
		cus := "cus_" + u()
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe"), Customer: &cus})
		r := race(t, func(q *sqlc.Queries) error {
			_, err := s01Bind(ctx, q, id, s01Ptr("stripe"), &cus, "razorpay", nil)
			return err
		}, "AdminClearBillingPin", func(q *sqlc.Queries) s01Result {
			n, err := s01Clear(ctx, q, id, s01Ptr("stripe"), &cus)
			return s01Result{rows: n, err: err}
		})
		if r.err != nil || r.rows != 0 {
			t.Fatalf("clear after the switch: rows=%d err=%v, want 0 rows", r.rows, r.err)
		}
		s01Want(t, s01Read(t, pool, id), "razorpay", "<NULL>", "<NULL>")
	})

	t.Run("clear_after_a_customer_fill_is_zero_rows", func(t *testing.T) {
		id := s01Seed(t, pool, s01Pin{Provider: s01Ptr("stripe")})
		cusB := "cus_" + u()
		r := race(t, func(q *sqlc.Queries) error {
			_, err := s01Bind(ctx, q, id, s01Ptr("stripe"), nil, "stripe", &cusB)
			return err
		}, "AdminClearBillingPin", func(q *sqlc.Queries) s01Result {
			n, err := s01Clear(ctx, q, id, s01Ptr("stripe"), nil)
			return s01Result{rows: n, err: err}
		})
		if r.err != nil || r.rows != 0 {
			t.Fatalf("clear after the fill: rows=%d err=%v, want 0 rows", r.rows, r.err)
		}
		s01Want(t, s01Read(t, pool, id), "stripe", cusB, "<NULL>")
	})
}
