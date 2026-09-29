package site

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	agentpkg "github.com/mosamlife/wpmgr/apps/api/internal/agent"
)

// recipientAlreadySetRepo is adoptRepo, except UpdateMetadata reports an
// AgeRecipient already on the site, so a metadata push with a different
// AgeRecipient always takes the rejected-change path in
// applyAgentAgeRecipient - the path that logs and audits agentVersion.
type recipientAlreadySetRepo struct{ *adoptRepo }

func (r recipientAlreadySetRepo) UpdateMetadata(ctx context.Context, tenantID, siteID uuid.UUID, m Metadata, b []byte) (Site, error) {
	s, err := r.adoptRepo.UpdateMetadata(ctx, tenantID, siteID, m, b)
	s.AgeRecipient = "age1storedrecipient"
	return s, err
}

// recordingHandler is an slog.Handler that keeps every record it is handed,
// so a test can inspect the attributes a log call actually carried.
type recordingHandler struct{ recs []slog.Record }

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.recs = append(h.recs, r)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// TestApplyAgentMetadata_AgentVersionCappedInLogAndAudit proves that an
// agent-reported version too large to be a real plugin version never reaches
// the rejected-recipient-change Warn log uncapped. ApplyAgentMetadata is the
// one path a hostile or malfunctioning agent can reach with metadata, and
// applyAgentAgeRecipient's rejection path (recordRecipientChangeRejected)
// logs and audits its agentVersion parameter with no bound of its own -
// both the slog.Warn call and the s.recordAudit call below it read that same
// parameter in that same call, so a capped log line is a capped audit
// metadata value too. The test only instruments the log: the audit recorder
// (*audit.Recorder) needs a live Postgres connection this package's unit
// tests do not have, and recordAudit is nil-safe and skipped without one.
func TestApplyAgentMetadata_AgentVersionCappedInLogAndAudit(t *testing.T) {
	clk := &manualClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	_, base := newAdoptService("https://example.com", &fakeProber{pingOK: true}, clk)
	svc := NewService(recipientAlreadySetRepo{base}, nil, clk)
	capture := &recordingHandler{}
	svc.SetLogger(slog.New(capture))

	huge := strings.Repeat("9", 1<<20) // 1 MiB, far past any real plugin version
	if _, err := svc.ApplyAgentMetadata(context.Background(), uuid.New(), uuid.New(), agentpkg.Metadata{
		AgeRecipient: "age1otherrecipient",
		AgentVersion: huge,
	}); err != nil {
		t.Fatalf("push: %v", err)
	}

	const maxLoggedBytes = maxAgentVersion * 4 // UTFMax; the probe's version is ASCII so this is generous

	found := false
	for _, r := range capture.recs {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key != "agent_version" {
				return true
			}
			found = true
			if n := len(a.Value.String()); n > maxLoggedBytes {
				t.Errorf("log %q: agent_version is %d bytes, over the %d-rune cap (%d bytes)", r.Message, n, maxAgentVersion, maxLoggedBytes)
			}
			return true
		})
	}
	if !found {
		t.Fatal("positive control: no log record carried agent_version; the rejection log path was not reached")
	}
}
