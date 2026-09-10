package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	agentpkg "github.com/mosamlife/wpmgr/apps/api/internal/agent"
)

// The agent metadata path writes sites.age_recipient, which names the age
// PUBLIC key a site's backups are encrypted to. These tests pin the three
// properties the write must have: a first set still lands, a change to an
// established recipient is refused AND visible, and a failed persist is not
// swallowed.

const (
	recipientA = "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqsaaaaaa"
	recipientB = "age1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzsbbbbbb"
)

// logCapture wires a service to a JSON logger writing into a buffer, so a test
// can assert on what the operator would actually see. The audit recorder needs
// a live pool, so the log line is the visibility surface a unit test can reach;
// the service emits it unconditionally, before and independently of the audit
// write.
func logCapture(svc *Service) *bytes.Buffer {
	var buf bytes.Buffer
	svc.SetLogger(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &buf
}

func applyRecipient(t *testing.T, svc *Service, tenantID, siteID uuid.UUID, rec string) error {
	t.Helper()
	_, err := svc.ApplyAgentMetadata(context.Background(), tenantID, siteID, agentpkg.Metadata{
		WPVersion:    "6.7",
		AgentVersion: "0.61.161",
		AgeRecipient: rec,
	})
	return err
}

// A first set is the path every normal enrolment takes: the column is empty and
// the agent's recipient must be recorded, or the site can never back up
// (internal/backup refuses with age_recipient_missing).
func TestAgentMetadataRecordsFirstAgeRecipient(t *testing.T) {
	repo := &fakeRepo{}
	svc := newSvc(repo)
	buf := logCapture(svc)
	tenantID, siteID := uuid.New(), uuid.New()

	if err := applyRecipient(t, svc, tenantID, siteID, recipientA); err != nil {
		t.Fatalf("ApplyAgentMetadata returned error on first set: %v", err)
	}

	if repo.ageRecipient != recipientA {
		t.Fatalf("first set was not persisted: column = %q, want %q", repo.ageRecipient, recipientA)
	}
	if repo.ifUnsetCalls != 1 {
		t.Fatalf("expected exactly one guarded write, got %d", repo.ifUnsetCalls)
	}
	if repo.unconditionalSets != 0 {
		t.Fatalf("agent path must never use the unconditional write, got %d calls", repo.unconditionalSets)
	}
	if !strings.Contains(buf.String(), "site backup recipient recorded") {
		t.Fatalf("first set was not logged; log = %s", buf.String())
	}
}

// Re-pushing the SAME recipient is the steady state (every metadata beat
// carries it). It must not re-write the column and must not look like a change.
func TestAgentMetadataSameRecipientIsANoOp(t *testing.T) {
	repo := &fakeRepo{ageRecipient: recipientA}
	svc := newSvc(repo)
	buf := logCapture(svc)

	if err := applyRecipient(t, svc, uuid.New(), uuid.New(), recipientA); err != nil {
		t.Fatalf("ApplyAgentMetadata returned error on unchanged recipient: %v", err)
	}
	if repo.ifUnsetCalls != 0 || repo.unconditionalSets != 0 {
		t.Fatalf("unchanged recipient triggered a write: ifUnset=%d unconditional=%d",
			repo.ifUnsetCalls, repo.unconditionalSets)
	}
	if strings.Contains(buf.String(), "rejected agent-pushed change") {
		t.Fatalf("unchanged recipient was reported as a rejected change; log = %s", buf.String())
	}
}

// The defect: a push carrying a DIFFERENT recipient silently replaced an
// established one. It must now leave the column alone and say so — a refused
// change that nobody can see is the same silence in a different shape.
func TestAgentMetadataRefusesAndReportsRecipientChange(t *testing.T) {
	repo := &fakeRepo{ageRecipient: recipientA}
	svc := newSvc(repo)
	buf := logCapture(svc)
	tenantID, siteID := uuid.New(), uuid.New()

	out, err := svc.ApplyAgentMetadata(context.Background(), tenantID, siteID, agentpkg.Metadata{
		WPVersion:    "6.7",
		AgentVersion: "0.61.161",
		AgeRecipient: recipientB,
	})
	if err != nil {
		// A refused change must not fail the whole metadata push: the rest of
		// the inventory is legitimate and the agent cannot fix the value by
		// retrying.
		t.Fatalf("ApplyAgentMetadata returned error on refused change: %v", err)
	}

	if repo.ageRecipient != recipientA {
		t.Fatalf("established recipient was overwritten: column = %q, want %q", repo.ageRecipient, recipientA)
	}
	if repo.unconditionalSets != 0 {
		t.Fatalf("refused change still reached the unconditional write (%d calls)", repo.unconditionalSets)
	}
	// The wire type carries no recipient field, so the site the agent gets back
	// is just the site; what matters is that the stored column is untouched
	// (asserted above) and the push still succeeded for every other field.
	if out.ID != siteID {
		t.Fatalf("returned site %v, want %v", out.ID, siteID)
	}

	// Visibility: one WARN line naming both recipients, so an operator reading
	// logs can tell which key the site's existing backups were written for and
	// which one is now claiming the site.
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] != "rejected agent-pushed change to an established site backup recipient" {
			continue
		}
		found = true
		if rec["level"] != "WARN" {
			t.Fatalf("rejection logged at %v, want WARN", rec["level"])
		}
		if rec["current_recipient"] != recipientA || rec["proposed_recipient"] != recipientB {
			t.Fatalf("rejection log lost the recipients: %v", rec)
		}
		if rec["site_id"] != siteID.String() {
			t.Fatalf("rejection log names site %v, want %s", rec["site_id"], siteID)
		}
	}
	if !found {
		t.Fatalf("a refused recipient change produced no log line; log = %s", buf.String())
	}
}

// The old code did `if err == nil { out = updated }`, so a failed write left the
// caller believing the recipient was stored. It must surface instead.
func TestAgentMetadataSurfacesRecipientPersistFailure(t *testing.T) {
	boom := errors.New("write failed")
	repo := &fakeRepo{ifUnsetErr: boom}
	svc := newSvc(repo)
	buf := logCapture(svc)

	err := applyRecipient(t, svc, uuid.New(), uuid.New(), recipientA)
	if err == nil {
		t.Fatalf("a failed recipient write was swallowed: ApplyAgentMetadata returned nil")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error did not carry the persist failure: %v", err)
	}
	if !strings.Contains(buf.String(), "failed to persist site age recipient") {
		t.Fatalf("persist failure was not logged; log = %s", buf.String())
	}
}

// Losing the in-transaction race to a concurrent first set is a refused change
// too: the column already holds someone else's value and this push must not
// pretend it won.
func TestAgentMetadataConcurrentFirstSetLoserIsReported(t *testing.T) {
	repo := &racingRepo{winner: recipientA}
	svc := newSvc(repo)
	buf := logCapture(svc)

	if err := applyRecipient(t, svc, uuid.New(), uuid.New(), recipientB); err != nil {
		t.Fatalf("ApplyAgentMetadata returned error for the race loser: %v", err)
	}
	if !strings.Contains(buf.String(), "concurrent_first_set") {
		t.Fatalf("race loser was not reported; log = %s", buf.String())
	}
}

// racingRepo simulates the other transaction winning the per-site advisory lock
// and setting the recipient first: the guarded write applies nothing and hands
// back the winner's row.
type racingRepo struct {
	fakeRepo
	winner string
}

func (r *racingRepo) SetAgeRecipientIfUnset(_ context.Context, tenantID, siteID uuid.UUID, _ string) (Site, bool, error) {
	r.ifUnsetCalls++
	r.ageRecipient = r.winner
	return Site{ID: siteID, TenantID: tenantID, AgeRecipient: r.winner}, false, nil
}
