package main

// email_log_gc_registration_test.go — mirrors
// email_webhook_gc_registration_test.go for registerEmailLogGCWorker (m59
// Phase 3 / PR #488 bot review). Same oracle: river.Workers has no exported
// way to list registered kinds, so a second, independent AddWorkerSafely
// call for "email_log_gc" only errors if the first call — going through
// registerEmailLogGCWorker, the same function startRiver calls — actually
// registered the kind.

import (
	"log/slog"
	"testing"

	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/email"
)

func TestRegisterEmailLogGCWorker(t *testing.T) {
	workers := river.NewWorkers()
	w := email.NewEmailLogGCWorker(nil, slog.Default())

	periodics, err := registerEmailLogGCWorker(workers, nil, w)
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
	dup := email.NewEmailLogGCWorker(nil, slog.Default())
	if err := river.AddWorkerSafely(workers, dup); err == nil {
		t.Fatal("expected AddWorkerSafely to reject a duplicate " +
			"\"email_log_gc\" registration, got nil error — " +
			"the worker was never actually registered")
	}
}

// TestRegisterEmailLogGCWorker_NilWorkerErrors is the PR #488 bot review fix
// applied to the sibling of registerEmailWebhookDedupGCWorker: the worker
// passed in is unconditionally constructed in run() (no feature flag gates
// it), so a nil worker here always means broken startup wiring, not an
// intentionally-disabled feature. It must error rather than silently no-op,
// so startRiver fails closed instead of starting with site_email_log
// retention silently dead.
func TestRegisterEmailLogGCWorker_NilWorkerErrors(t *testing.T) {
	workers := river.NewWorkers()

	periodics, err := registerEmailLogGCWorker(workers, nil, nil)
	if err == nil {
		t.Fatal("expected a non-nil error for a nil worker, got nil — " +
			"a nil worker must fail startup, not silently skip registration")
	}

	if len(periodics) != 0 {
		t.Fatalf("expected no periodic job for a nil worker, got %d", len(periodics))
	}
}
