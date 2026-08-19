package email

// worker_test.go — GH #461: locks the documented webhook dedup retention
// window (7 days) referenced by the migration comment at
// apps/api/migrations/20260623000000_m60_email_webhook_dedup.sql:14
// ("Retention: rows older than 7 days are pruned by the
// EmailWebhookDedupGCWorker") and by WebhookDedupGCWorker.Work, which
// computes its cutoff as time.Now().UTC().Add(-webhookDedupRetention).

import (
	"testing"
	"time"
)

func TestWebhookDedupRetentionWindow(t *testing.T) {
	const want = 7 * 24 * time.Hour
	if webhookDedupRetention != want {
		t.Fatalf("webhookDedupRetention = %s, want %s (documented 7-day retention window)",
			webhookDedupRetention, want)
	}
}
