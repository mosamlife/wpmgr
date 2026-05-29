package diagnostics

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Service is a thin orchestrator: Repo + RefreshEnqueuer (optional). Held
// stateless so handlers can compose it freely.
type Service struct {
	repo     *Repo
	enqueuer RefreshEnqueuer
}

// RefreshEnqueuer enqueues an on-demand diagnostics command to the agent.
// Optional; when nil the /diagnostics/refresh endpoint returns a 503 pointing
// at the missing wire (mirrors the InspectionDeps pattern in backups).
type RefreshEnqueuer interface {
	EnqueueRefreshDiagnostics(ctx context.Context, tenantID, siteID uuid.UUID) error
}

// NewService builds a Service.
func NewService(repo *Repo) *Service {
	return &Service{repo: repo}
}

// SetRefreshEnqueuer wires the on-demand refresh enqueuer once it is built
// (the enqueuer needs the River client; the service is built before River
// starts).
func (s *Service) SetRefreshEnqueuer(e RefreshEnqueuer) {
	s.enqueuer = e
}

// IngestDiagnostics splits the agent-shipped 14-category blob into one
// upsert per category. The blob is shaped as
//
//	{
//	  "identity": {...},
//	  "php": {...},
//	  ...
//	  "collected_at": 1748505600
//	}
//
// We extract `collected_at` (agent-side Unix seconds) and apply it to every
// category's row so a tab of cards can render a single "as of" timestamp.
func (s *Service) IngestDiagnostics(ctx context.Context, tenantID, siteID uuid.UUID, body []byte) (int, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, err
	}
	collected := agentCollectedAt(raw)
	count := 0
	for _, cat := range AllCategories() {
		payload, ok := raw[string(cat)]
		if !ok || len(payload) == 0 {
			continue
		}
		if _, err := s.repo.UpsertDiagnostic(ctx, tenantID, siteID, cat, payload, collected); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// LatestBySite returns a map keyed by category string. Categories the agent
// has not yet shipped are present in the map only when stored — the handler
// fills in "awaiting first sync" placeholders for missing ones.
func (s *Service) LatestBySite(ctx context.Context, tenantID, siteID uuid.UUID) (map[Category]Diagnostic, error) {
	rows, err := s.repo.ListDiagnosticsBySite(ctx, tenantID, siteID)
	if err != nil {
		return nil, err
	}
	out := make(map[Category]Diagnostic, len(rows))
	for _, r := range rows {
		out[r.Category] = r
	}
	return out, nil
}

// IngestErrorBatch takes the agent-shipped batch of newest unsilenced rows
// and upserts each. Returns the highest agent-side row id we processed (the
// agent uses this to advance its local ship cursor on a 2xx).
type ErrorBatchEntry struct {
	ID              int64  `json:"id"`
	MD5             string `json:"md5"`
	Code            int    `json:"code"`
	Severity        string `json:"severity"`
	Message         string `json:"message"`
	File            string `json:"file"`
	Line            int    `json:"line"`
	RequestPath     string `json:"request_path"`
	FirstSeen       int64  `json:"first_seen"`
	LastSeen        int64  `json:"last_seen"`
	OccurrenceCount int64  `json:"occurrence_count"`
}

type ErrorBatch struct {
	Errors []ErrorBatchEntry `json:"errors"`
}

func (s *Service) IngestErrorBatch(ctx context.Context, tenantID, siteID uuid.UUID, batch ErrorBatch) (int64, error) {
	var highest int64
	for _, e := range batch.Errors {
		if e.MD5 == "" {
			continue
		}
		if err := s.repo.UpsertPHPError(ctx, tenantID, siteID, UpsertPHPErrorInput{
			MD5:             e.MD5,
			Code:            e.Code,
			Severity:        coalesce(e.Severity, "warning"),
			Message:         e.Message,
			File:            e.File,
			Line:            e.Line,
			RequestPath:     e.RequestPath,
			FirstSeenAt:     time.Unix(e.FirstSeen, 0).UTC(),
			LastSeenAt:      time.Unix(e.LastSeen, 0).UTC(),
			OccurrenceCount: e.OccurrenceCount,
			AgentRowID:      e.ID,
		}); err != nil {
			return highest, err
		}
		if e.ID > highest {
			highest = e.ID
		}
	}
	return highest, nil
}

// ListErrors passes through to the repo.
func (s *Service) ListErrors(ctx context.Context, tenantID, siteID uuid.UUID, f ListPHPErrorsFilter) ([]PHPError, error) {
	return s.repo.ListPHPErrorsBySite(ctx, tenantID, siteID, f)
}

// SetSilenced passes through.
func (s *Service) SetSilenced(ctx context.Context, tenantID, siteID uuid.UUID, md5 string, silenced bool) error {
	return s.repo.SetSilenced(ctx, tenantID, siteID, md5, silenced)
}

// RefreshAgent fires the on-demand command. Returns nil on success; when the
// enqueuer is unwired it returns a stable "feature unwired" error the handler
// turns into a 503.
func (s *Service) RefreshAgent(ctx context.Context, tenantID, siteID uuid.UUID) error {
	if s.enqueuer == nil {
		return errUnwired
	}
	return s.enqueuer.EnqueueRefreshDiagnostics(ctx, tenantID, siteID)
}

// agentCollectedAt pulls the agent-side collection timestamp out of the
// payload. Falls back to "now" if the agent omitted it.
func agentCollectedAt(raw map[string]json.RawMessage) time.Time {
	v, ok := raw["collected_at"]
	if !ok || len(v) == 0 {
		return time.Now().UTC()
	}
	var ts int64
	if err := json.Unmarshal(v, &ts); err != nil || ts <= 0 {
		return time.Now().UTC()
	}
	return time.Unix(ts, 0).UTC()
}

func coalesce(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// errUnwired is returned by RefreshAgent when no enqueuer is wired. The
// handler maps it to a 503 with a stable code.
var errUnwired = &unwiredError{}

type unwiredError struct{}

func (u *unwiredError) Error() string { return "diagnostics_refresh_unwired" }
