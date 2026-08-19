package main

// email_webhook_gc_registration_test.go — GH #461 regression lock: the
// email_webhook_dedup_gc River worker was fully implemented in
// internal/email/worker.go but never registered in startRiver, so
// email_webhook_events grew without bound. river.Workers has no exported
// way to list registered kinds, and startRiver itself needs a live
// *pgxpool.Pool, so this proves the registration through the one available
// oracle: river.AddWorker panics (and its non-panicking twin,
// AddWorkerSafely, errors) if the same job kind is registered twice on the
// same *river.Workers. A second, independent AddWorkerSafely call for
// "email_webhook_dedup_gc" only errors if the first call — going through
// registerEmailWebhookDedupGCWorker, the same function startRiver calls —
// actually registered the kind.

import (
	"log/slog"
	"testing"

	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/email"
)

func TestRegisterEmailWebhookDedupGCWorker(t *testing.T) {
	workers := river.NewWorkers()
	w := email.NewWebhookDedupGCWorker(nil, slog.Default())

	periodics, err := registerEmailWebhookDedupGCWorker(workers, nil, w)
	if err != nil {
		t.Fatalf("expected nil error for a non-nil worker, got: %v", err)
	}

	if len(periodics) != 1 {
		t.Fatalf("expected 1 periodic job appended, got %d", len(periodics))
	}

	// The real proof: the worker must actually be in the registry, not just
	// that the function ran without crashing. A second, independent
	// registration attempt for the same kind on the same *river.Workers must
	// fail.
	dup := email.NewWebhookDedupGCWorker(nil, slog.Default())
	if err := river.AddWorkerSafely(workers, dup); err == nil {
		t.Fatal("expected AddWorkerSafely to reject a duplicate " +
			"\"email_webhook_dedup_gc\" registration, got nil error — " +
			"the worker was never actually registered")
	}
}

// TestRegisterEmailWebhookDedupGCWorker_NilWorkerErrors is the PR #488 bot
// review fix: registerEmailWebhookDedupGCWorker(...) is called with a worker
// that is unconditionally constructed in run() (no feature flag gates it),
// so a nil worker here always means broken startup wiring, never an
// intentionally-disabled feature. The function used to swallow that case as
// a silent no-op and let startRiver succeed anyway — the exact "guard that
// finds nothing must go red, not green" failure GH #461 exists to prevent,
// one level up. It must now surface an error so startRiver fails closed.
func TestRegisterEmailWebhookDedupGCWorker_NilWorkerErrors(t *testing.T) {
	workers := river.NewWorkers()

	periodics, err := registerEmailWebhookDedupGCWorker(workers, nil, nil)
	if err == nil {
		t.Fatal("expected a non-nil error for a nil worker, got nil — " +
			"a nil worker must fail startup, not silently skip registration")
	}

	if len(periodics) != 0 {
		t.Fatalf("expected no periodic job for a nil worker, got %d", len(periodics))
	}
}
