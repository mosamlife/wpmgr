package stripe

import (
	"context"
	"net/http"
	"testing"
	"time"

	stripesdk "github.com/stripe/stripe-go/v86"
)

// TestToSubscription_CancelSchedule proves the cancel schedule is read from
// both of Stripe's fields: a period-end cancel and a cancel set for a date
// both count as scheduled, and cancel_at is carried as an instant.
func TestToSubscription_CancelSchedule(t *testing.T) {
	p := New(testConfig())
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		periodEnd     bool
		cancelAt      int64
		wantScheduled bool
		wantAt        time.Time
	}{
		{"none", false, 0, false, time.Time{}},
		{"period end only", true, 0, true, time.Time{}},
		{"cancel_at only", false, at.Unix(), true, at},
		{"both", true, at.Unix(), true, at},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.toSubscription(&stripesdk.Subscription{
				ID:                "sub_c",
				Status:            stripesdk.SubscriptionStatusActive,
				CancelAtPeriodEnd: tt.periodEnd,
				CancelAt:          tt.cancelAt,
			})
			if got.CancelAtPeriodEnd != tt.wantScheduled {
				t.Errorf("CancelAtPeriodEnd = %v, want %v", got.CancelAtPeriodEnd, tt.wantScheduled)
			}
			if !got.CancelAt.Equal(tt.wantAt) {
				t.Errorf("CancelAt = %v, want %v", got.CancelAt, tt.wantAt)
			}
			if !got.CancelScheduleReported {
				t.Error("CancelScheduleReported = false; Stripe reports its schedule on the subscription")
			}
		})
	}
}

// TestCancelSubscriptionNow_SendsDelete proves Cancel now is one DELETE of
// the named subscription, with no proration credit and no final invoice.
func TestCancelSubscriptionNow_SendsDelete(t *testing.T) {
	p, rt := newRecordingProvider(func(*http.Request) (int, string) {
		return 200, `{"id":"sub_now","object":"subscription","status":"canceled"}`
	})
	if err := p.CancelSubscriptionNow(context.Background(), "sub_now"); err != nil {
		t.Fatalf("CancelSubscriptionNow: %v", err)
	}
	reqs := rt.seen()
	if len(reqs) != 1 {
		t.Fatalf("requests sent = %d, want 1", len(reqs))
	}
	if reqs[0].Method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", reqs[0].Method)
	}
	if reqs[0].URL.Path != "/v1/subscriptions/sub_now" {
		t.Errorf("path = %s, want /v1/subscriptions/sub_now", reqs[0].URL.Path)
	}
	params := cancelNowParams()
	if params.Prorate == nil || *params.Prorate {
		t.Error("Prorate must be sent as false")
	}
	if params.InvoiceNow == nil || *params.InvoiceNow {
		t.Error("InvoiceNow must be sent as false")
	}
}

// TestCancelSubscriptionNow_EmptyIDSendsNothing proves an empty id is refused
// before any request is sent.
func TestCancelSubscriptionNow_EmptyIDSendsNothing(t *testing.T) {
	p, rt := newRecordingProvider(func(*http.Request) (int, string) {
		return 200, `{}`
	})
	if err := p.CancelSubscriptionNow(context.Background(), ""); err == nil {
		t.Fatal("an empty subscription id must be refused")
	}
	if n := len(rt.seen()); n != 0 {
		t.Fatalf("requests sent = %d, want 0", n)
	}
}
