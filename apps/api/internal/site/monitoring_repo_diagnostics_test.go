// monitoring_repo_diagnostics_test.go — GH #414 review finding: PauseMonitoring
// and ResumeMonitoring collapsed every tx failure into the same generic
// domain.Internal error with nothing logged, so a check violation, a
// foreign-key violation, a deadlock, a context cancellation and a plain
// connection failure were all indistinguishable in production. These are
// pure unit tests against monitoringTxFailureCause/logMonitoringTxFailure —
// no database needed — because the classification and the log line are the
// whole fix; the client-facing error code is untouched (see
// monitoring_repo.go and requireDomainCode-style assertions elsewhere in this
// package that key on it).
package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMonitoringTxFailureCause(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantCause string
		wantState string
		wantPgMsg string
	}{
		{
			name:      "nil principal invariant",
			err:       errMonitoringPrincipalRequired,
			wantCause: "nil_principal_invariant",
		},
		{
			name:      "context canceled",
			err:       context.Canceled,
			wantCause: "context_canceled",
		},
		{
			name:      "context deadline exceeded",
			err:       context.DeadlineExceeded,
			wantCause: "context_deadline_exceeded",
		},
		{
			name:      "check violation",
			err:       &pgconn.PgError{Code: "23514", Message: "new row violates check constraint"},
			wantCause: "pg_error",
			wantState: "23514",
			wantPgMsg: "new row violates check constraint",
		},
		{
			name:      "foreign key violation",
			err:       &pgconn.PgError{Code: "23503", Message: "insert or update violates foreign key constraint"},
			wantCause: "pg_error",
			wantState: "23503",
			wantPgMsg: "insert or update violates foreign key constraint",
		},
		{
			name:      "deadlock",
			err:       &pgconn.PgError{Code: "40P01", Message: "deadlock detected"},
			wantCause: "pg_error",
			wantState: "40P01",
			wantPgMsg: "deadlock detected",
		},
		{
			name:      "plain connection failure",
			err:       errors.New("connect: connection refused"),
			wantCause: "unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cause, sqlstate, pgMessage := monitoringTxFailureCause(tc.err)
			if cause != tc.wantCause {
				t.Fatalf("cause = %q, want %q", cause, tc.wantCause)
			}
			if sqlstate != tc.wantState {
				t.Fatalf("sqlstate = %q, want %q", sqlstate, tc.wantState)
			}
			if pgMessage != tc.wantPgMsg {
				t.Fatalf("pgMessage = %q, want %q", pgMessage, tc.wantPgMsg)
			}
		})
	}
}

// captureSlogDefault swaps slog's default logger for one writing JSON lines
// into buf, and restores the previous default on cleanup.
func captureSlogDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestLogMonitoringTxFailure_EmitsDiagnosticLogLine is the failing-before /
// passing-after proof for the review finding: before this fix PauseMonitoring
// and ResumeMonitoring called neither this function nor anything like it, so
// a tx failure left no trace at all. Asserting the log line exists, and
// carries the SQLSTATE plus a bounded tenant_id/site_count (never the site
// ids or the pause reason), is what "preserve the cause for the logs"
// requires.
func TestLogMonitoringTxFailure_EmitsDiagnosticLogLine(t *testing.T) {
	buf := captureSlogDefault(t)
	tenantID := uuid.New()
	pgErr := &pgconn.PgError{Code: "23503", Message: "insert or update violates foreign key constraint",
		Detail: "Key (monitoring_paused_by)=(11111111-1111-1111-1111-111111111111) is not present in table \"users\"."}

	logMonitoringTxFailure(context.Background(), "pause", tenantID, 3, pgErr)

	line := buf.String()
	if line == "" {
		t.Fatalf("expected a log line, got none")
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
	}
	if got["msg"] != "monitoring: tx failed" {
		t.Fatalf("msg = %v, want %q", got["msg"], "monitoring: tx failed")
	}
	if got["op"] != "pause" {
		t.Fatalf("op = %v, want %q", got["op"], "pause")
	}
	if got["tenant_id"] != tenantID.String() {
		t.Fatalf("tenant_id = %v, want %q", got["tenant_id"], tenantID.String())
	}
	if got["cause"] != "pg_error" {
		t.Fatalf("cause = %v, want %q", got["cause"], "pg_error")
	}
	if got["sqlstate"] != "23503" {
		t.Fatalf("sqlstate = %v, want %q", got["sqlstate"], "23503")
	}
	siteCount, ok := got["site_count"].(float64)
	if !ok || siteCount != 3 {
		t.Fatalf("site_count = %v, want 3", got["site_count"])
	}
	// The FK violation's Detail carries a row value (a user id here, but the
	// same field carries site data on other constraints); it must never reach
	// the log line even though pgconn attaches it to the error.
	if bytes.Contains(buf.Bytes(), []byte("11111111-1111-1111-1111-111111111111")) {
		t.Fatalf("log line leaked the pg error Detail field: %s", line)
	}
}

// TestLogMonitoringTxFailure_NilPrincipalIsDistinguishedFromDBFailure proves
// the errMonitoringPrincipalRequired branch does not fall into the generic
// pg-error/unknown path: an on-call reader must be able to tell "the
// service-layer guard was bypassed" apart from "the database failed" without
// decoding a SQLSTATE.
func TestLogMonitoringTxFailure_NilPrincipalIsDistinguishedFromDBFailure(t *testing.T) {
	buf := captureSlogDefault(t)
	logMonitoringTxFailure(context.Background(), "resume", uuid.New(), 1, errMonitoringPrincipalRequired)

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line is not valid JSON: %v", err)
	}
	if got["cause"] != "nil_principal_invariant" {
		t.Fatalf("cause = %v, want %q", got["cause"], "nil_principal_invariant")
	}
	if _, hasSQLState := got["sqlstate"]; hasSQLState {
		t.Fatalf("nil-principal failure must not carry a sqlstate field, got %v", got)
	}
}
