package mcp

// Unit proofs for the AI request cascade inside RevokeConnection. The Service
// is built with a store and an audit recorder and nothing else: no write-tools
// switch, no assistantrequest service, no worker. That is the point. The
// cascade is part of the revoke, not something wired in beside it.
//
// The fake reports which request ids the cascade closed; which rows those are
// (waiting and approved-but-not-reserved, never reserved) is a property of the
// SQL and is proven as wpmgr_app in
// apps/api/tests/mcp_revoke_cascade_integration_test.go.

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// revokers is the two credential kinds that can revoke a connection: a signed-in
// person and an org-scoped API key.
func revokers(tenant uuid.UUID) []struct {
	name     string
	p        domain.Principal
	wantType string
	wantID   string
} {
	user := orgPrincipal(tenant)
	key := apiKeyPrincipal(tenant)
	return []struct {
		name     string
		p        domain.Principal
		wantType string
		wantID   string
	}{
		{"user", user, audit.ActorUser, user.UserID.String()},
		{"api key", key, audit.ActorAPIKey, key.APIKeyID.String()},
	}
}

// TestRevokeCascade_OneAuditRowPerClosedRequestAsTheRevoker: every request the
// cascade closed gets exactly one audit row, in the revoke's transaction,
// naming the credential that revoked; an API key is recorded as the key, never
// as a nil user. The request rows come before the revoke's own row.
func TestRevokeCascade_OneAuditRowPerClosedRequestAsTheRevoker(t *testing.T) {
	tenant := uuid.New()
	for _, tc := range revokers(tenant) {
		t.Run(tc.name, func(t *testing.T) {
			grantID := uuid.New()
			w1, w2, n1 := uuid.New(), uuid.New(), uuid.New()
			store := &fakeStore{
				revokeRow:        sqlc.RevokeMCPGrantWithTokensInTenantTxRow{GrantsRevoked: 1, TokensRevoked: 1},
				cascadeWithdrawn: []uuid.UUID{w1, w2},
				cascadeNotSent:   []uuid.UUID{n1},
			}
			rec := &capturingRecorder{}
			svc := NewService(store).withAuditRecorder(rec)

			if _, err := svc.RevokeConnection(context.Background(), tc.p, grantID); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			if store.cascadeCalls != 1 {
				t.Fatalf("the cascade ran %d times, want exactly 1", store.cascadeCalls)
			}
			if !slices.Contains(store.callLog(), "CloseAssistantRequestsForGrantTx") {
				t.Fatalf("the cascade was not called through the store: %v", store.callLog())
			}

			// The type and id come from audit.ActorFor, and are pinned to the
			// literal expectation too, so a change to ActorFor that recorded a
			// key as a user would not pass by agreeing with itself.
			gotType, gotID := audit.ActorFor(tc.p)
			if gotType != tc.wantType || gotID != tc.wantID {
				t.Fatalf("audit.ActorFor(%s) = %s/%s, want %s/%s", tc.name, gotType, gotID, tc.wantType, tc.wantID)
			}
			if tc.wantID == uuid.Nil.String() {
				t.Fatal("fixture revoker has a nil id; the actor assertion below would prove nothing")
			}

			rec.mu.Lock()
			events := append([]audit.Event(nil), rec.events...)
			rec.mu.Unlock()

			type want struct {
				action, target string
				md             map[string]string
			}
			wants := []want{
				{audit.ActionAssistantRequestWithdrawn, w1.String(), map[string]string{
					"reason": "connection_revoked", "proposed_by_grant_id": grantID.String()}},
				{audit.ActionAssistantRequestWithdrawn, w2.String(), map[string]string{
					"reason": "connection_revoked", "proposed_by_grant_id": grantID.String()}},
				{audit.ActionAssistantRequestNotSent, n1.String(), map[string]string{
					"reason": "grant_inactive", "closed_by": "connection_revoked", "proposed_by_grant_id": grantID.String()}},
				{audit.ActionMCPGrantRevoked, grantID.String(), nil},
			}
			if len(events) != len(wants) {
				t.Fatalf("recorded %d audit rows, want %d (one per closed request, then the revoke): %+v",
					len(events), len(wants), events)
			}
			for i, w := range wants {
				e := events[i]
				if e.Action != w.action || e.TargetID != w.target {
					t.Errorf("row %d: %s on %s, want %s on %s", i, e.Action, e.TargetID, w.action, w.target)
				}
				if e.ActorType != tc.wantType || e.ActorID != tc.wantID {
					t.Errorf("row %d (%s): actor %s/%s, want the revoker %s/%s",
						i, e.Action, e.ActorType, e.ActorID, tc.wantType, tc.wantID)
				}
				if e.TenantID != tenant {
					t.Errorf("row %d (%s): tenant %s, want %s", i, e.Action, e.TenantID, tenant)
				}
				for k, v := range w.md {
					if got, _ := e.Metadata[k].(string); got != v {
						t.Errorf("row %d (%s): metadata %s = %v, want %q", i, e.Action, k, e.Metadata[k], v)
					}
				}
				if w.action != audit.ActionMCPGrantRevoked && e.TargetType != "assistant_cache_purge_request" {
					t.Errorf("row %d (%s): target type %q, want assistant_cache_purge_request", i, e.Action, e.TargetType)
				}
			}
		})
	}
}

// TestRevokeCascade_NothingClosedWritesOnlyTheRevokeRow is the over-fire
// check: a grant with no waiting or approved requests, and a repeated revoke,
// write the revoke's own row and no request rows.
func TestRevokeCascade_NothingClosedWritesOnlyTheRevokeRow(t *testing.T) {
	tenant := uuid.New()
	store := &fakeStore{} // an already-revoked grant: two zeroes, success
	rec := &capturingRecorder{}
	svc := NewService(store).withAuditRecorder(rec)

	if _, err := svc.RevokeConnection(context.Background(), orgPrincipal(tenant), uuid.New()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if store.cascadeCalls != 1 {
		t.Fatalf("the cascade ran %d times, want 1 (it runs on every non-error outcome)", store.cascadeCalls)
	}
	rec.only(t, audit.ActionMCPGrantRevoked)
	if len(rec.events) != 1 {
		t.Fatalf("recorded %d rows with nothing closed, want only the revoke row: %+v", len(rec.events), rec.events)
	}
}

// TestRevokeCascade_ACascadeErrorFailsTheRevoke: the cascade shares the
// revoke's transaction, so a cascade failure fails the revoke. It is never
// swallowed, and nothing is recorded as revoked.
func TestRevokeCascade_ACascadeErrorFailsTheRevoke(t *testing.T) {
	tenant := uuid.New()
	for _, tc := range revokers(tenant) {
		t.Run(tc.name, func(t *testing.T) {
			planted := errors.New("planted cascade failure")
			store := &fakeStore{
				revokeRow:        sqlc.RevokeMCPGrantWithTokensInTenantTxRow{GrantsRevoked: 1, TokensRevoked: 1},
				cascadeWithdrawn: []uuid.UUID{uuid.New()},
				cascadeErr:       planted,
			}
			rec := &capturingRecorder{}
			svc := NewService(store).withAuditRecorder(rec)

			out, err := svc.RevokeConnection(context.Background(), tc.p, uuid.New())
			if err == nil {
				t.Fatalf("the revoke succeeded (%+v) although its cascade failed", out)
			}
			if !errors.Is(err, planted) {
				t.Fatalf("revoke error %v does not carry the cascade's error", err)
			}
			if _, ok := domain.AsDomain(err); ok {
				t.Fatalf("a cascade failure surfaced as a domain error (%v); it is an infra failure", err)
			}
			if out != (RevokeOutcome{}) {
				t.Fatalf("a failed revoke reported an outcome: %+v", out)
			}
			if store.cascadeCalls != 1 {
				t.Fatalf("the cascade ran %d times, want 1", store.cascadeCalls)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.events) != 0 {
				t.Fatalf("a failed revoke recorded %d audit rows: %+v", len(rec.events), rec.events)
			}
		})
	}
}
