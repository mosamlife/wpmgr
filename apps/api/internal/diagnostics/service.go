package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/ipprovider"
)

// HostResolver infers a hosting provider from an IP (M28). *ipprovider.Resolver
// satisfies it. Declared as an optional interface so the service composes with
// or without the feature and tests can substitute a fake.
type HostResolver interface {
	Resolve(ip string) ipprovider.Result
}

// AgentErrorConfigClient is the subset of agentcmd.Client the service needs to
// push an error config to the agent. *agentcmd.Client satisfies it via its
// SyncErrorConfig method. Declared as an interface so tests can substitute a
// fake without spinning up the SSRF transport.
type AgentErrorConfigClient interface {
	SyncErrorConfig(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.ErrorConfigRequest) (agentcmd.ErrorConfigResult, error)
}

// Service is a thin orchestrator: Repo + RefreshEnqueuer (optional) +
// AgentErrorConfigClient (optional). Held stateless so handlers can compose
// it freely.
type Service struct {
	repo          *Repo
	enqueuer      RefreshEnqueuer
	errorClient   AgentErrorConfigClient
	siteLookup    SiteLookup
	hostResolver  HostResolver
	dbSizeHistory DBSizeHistorySink
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

// SetHostResolver wires the offline IP -> hosting-provider resolver (M28).
// Optional: when nil, ResolveHostProvider is a no-op and host_provider stays "".
func (s *Service) SetHostResolver(r HostResolver) {
	s.hostResolver = r
}

// ResolveHostProvider infers and records the site's hosting provider from the
// agent's egress IP (M28). Best-effort and non-fatal, called after a diagnostics
// push. It prefers the CP-observed source IP, falling back to the agent-reported
// public IP (hosting.public_ip, fetched by the agent from ipify) ONLY when the
// observed IP is not globally routable — the self-host-behind-proxy / same-LAN
// case. The result is recorded even when the provider is unrecognized so the
// observed IP, raw network org, and checked-at are stored (auditable + visible),
// and a changed IP re-resolves on the next push.
func (s *Service) ResolveHostProvider(ctx context.Context, tenantID, siteID uuid.UUID, observedIP string, body []byte) {
	if s.hostResolver == nil {
		return
	}
	ip := observedIP
	if !ipprovider.IsGlobalUnicast(observedIP) {
		if reported := reportedPublicIP(body); ipprovider.IsGlobalUnicast(reported) {
			ip = reported
		}
	}
	if ip == "" {
		return
	}
	res := s.hostResolver.Resolve(ip)
	_ = s.repo.SetSiteHostProvider(ctx, tenantID, siteID, res.Provider, res.ASOrg, ip)
}

// reportedPublicIP extracts the agent-reported public IP (M28 fallback) from the
// diagnostics body's hosting category (hosting.public_ip). Returns "" when
// absent or malformed.
func reportedPublicIP(body []byte) string {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return ""
	}
	h, ok := raw["hosting"]
	if !ok {
		return ""
	}
	var hosting struct {
		PublicIP string `json:"public_ip"`
	}
	if json.Unmarshal(h, &hosting) != nil {
		return ""
	}
	return hosting.PublicIP
}

// SetRefreshEnqueuer wires the on-demand refresh enqueuer once it is built
// (the enqueuer needs the River client; the service is built before River
// starts).
func (s *Service) SetRefreshEnqueuer(e RefreshEnqueuer) {
	s.enqueuer = e
}

// SetErrorConfigClient wires the agentcmd client for pushing error config to
// the agent. Must be called before any SyncErrorConfig call. The SiteLookup
// is required alongside it so the service can resolve the site URL without a
// hard dependency on the site package.
func (s *Service) SetErrorConfigClient(client AgentErrorConfigClient, sites SiteLookup) {
	s.errorClient = client
	s.siteLookup = sites
}

// md5Re validates a 32-character lowercase hex md5 fingerprint.
var md5Re = regexp.MustCompile(`^[0-9a-f]{32}$`)

// GetErrorConfig returns the stored error config for (tenantID, siteID).
// When no row exists yet it returns the default (error_level=6143, empty list).
func (s *Service) GetErrorConfig(ctx context.Context, tenantID, siteID uuid.UUID) (ErrorConfig, error) {
	cfg, found, err := s.repo.GetErrorConfig(ctx, tenantID, siteID)
	if err != nil {
		return ErrorConfig{}, err
	}
	if !found {
		return ErrorConfig{
			TenantID:   tenantID,
			SiteID:     siteID,
			Enabled:    true,
			ErrorLevel: agentcmd.DefaultErrorLevel,
			IgnoreMD5s: []string{},
		}, nil
	}
	return cfg, nil
}

// SaveErrorConfig validates the new config, upserts it in the database, and
// pushes it to the agent via the sync_error_config command. Returns the stored
// config. If the agentcmd client is not wired the upsert still succeeds and
// the config is stored — the push is skipped with a logged warning rather
// than a hard failure (the operator can trigger a manual re-sync later).
func (s *Service) SaveErrorConfig(ctx context.Context, tenantID, siteID uuid.UUID, cfg ErrorConfig) (ErrorConfig, error) {
	// --- validation ---
	if cfg.ErrorLevel <= 0 {
		return ErrorConfig{}, domain.Validation("invalid_error_level", "error_level must be a positive PHP E_* bitmask")
	}
	if cfg.ErrorLevel > (1<<31 - 1) {
		return ErrorConfig{}, domain.Validation("invalid_error_level", "error_level overflows int32")
	}
	if cfg.IgnoreMD5s == nil {
		cfg.IgnoreMD5s = []string{}
	}
	for _, m := range cfg.IgnoreMD5s {
		if !md5Re.MatchString(m) {
			return ErrorConfig{}, domain.Validation("invalid_md5", fmt.Sprintf("ignore_md5s entry %q is not a valid 32-char lowercase hex md5", m))
		}
	}
	cfg.TenantID = tenantID
	cfg.SiteID = siteID

	// --- persist ---
	saved, err := s.repo.UpsertErrorConfig(ctx, cfg)
	if err != nil {
		return ErrorConfig{}, err
	}

	// --- push to agent (best-effort) ---
	if s.errorClient != nil && s.siteLookup != nil {
		siteURL, lookupErr := s.siteLookup.GetSiteURL(ctx, tenantID, siteID)
		if lookupErr == nil {
			md5s := saved.IgnoreMD5s
			if md5s == nil {
				md5s = []string{}
			}
			if _, pushErr := s.errorClient.SyncErrorConfig(ctx, siteID, siteURL, agentcmd.ErrorConfigRequest{
				Enabled:    saved.Enabled,
				ErrorLevel: saved.ErrorLevel,
				IgnoreMD5s: md5s,
			}); pushErr != nil {
				// Non-fatal: the config is already persisted. The agent will
				// pick it up on next sync or when the operator re-saves.
				// We return the stored config + the push error wrapped so
				// the handler can surface it as a 207-style warning if desired.
				return saved, fmt.Errorf("config stored but agent push failed: %w", pushErr)
			}
		}
		// site URL lookup failure is also non-fatal — config is still stored.
	}

	return saved, nil
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

		// At identity ingest, persist the WordPress timezone onto the site row
		// so the backup scheduler can resolve the correct time zone without a
		// separate lookup. Failures here are non-fatal: a malformed identity
		// payload must not abort the whole diagnostics ingest.
		if cat == CategoryIdentity {
			s.ingestSiteTimezone(ctx, tenantID, siteID, payload)
		}

		// At wp_native ingest, append a DB-size trend point from the
		// wp-paths-sizes.fields.database_size the agent already computes for
		// the Directory Sizes card (GH #196). Best-effort/non-fatal — see
		// ingestDBSizeHistory (db_size_history.go).
		if cat == CategoryWPNative {
			s.ingestDBSizeHistory(ctx, tenantID, siteID, payload, collected)
		}
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
//
// Wire shape: wpdb ARRAY_A delivers every column as a string, so the numeric
// scalars (id, code, line, first_seen, last_seen, occurrence_count) arrive
// as either a Go number (when the caller is a test) or a quoted numeric string
// (when the agent serialises its ARRAY_A result with json_encode). We decode
// them via flexInt64 / flexInt so both paths work without panicking.
//
// backtrace is a JSON array of {file,line,function} objects (max 10 frames,
// most-recent-call-first). Missing or null → treated as empty slice.
type ErrorBatchEntry struct {
	ID              flexInt64 `json:"id"`
	MD5             string    `json:"md5"`
	Code            flexInt   `json:"code"`
	Severity        string    `json:"severity"`
	Message         string    `json:"message"`
	File            string    `json:"file"`
	Line            flexInt   `json:"line"`
	RequestPath     string    `json:"request_path"`
	FirstSeen       flexInt64 `json:"first_seen"`
	LastSeen        flexInt64 `json:"last_seen"`
	OccurrenceCount flexInt64 `json:"occurrence_count"`
	Backtrace       []struct {
		File     string  `json:"file"`
		Line     flexInt `json:"line"`
		Function string  `json:"function"`
	} `json:"backtrace"`
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
		frames := make([]ErrorFrame, 0, len(e.Backtrace))
		for _, f := range e.Backtrace {
			frames = append(frames, ErrorFrame{
				File:     f.File,
				Line:     int(f.Line),
				Function: f.Function,
			})
		}
		if err := s.repo.UpsertPHPError(ctx, tenantID, siteID, UpsertPHPErrorInput{
			MD5:             e.MD5,
			Code:            int(e.Code),
			Severity:        coalesce(e.Severity, "warning"),
			Message:         e.Message,
			File:            e.File,
			Line:            int(e.Line),
			RequestPath:     e.RequestPath,
			FirstSeenAt:     time.Unix(int64(e.FirstSeen), 0).UTC(),
			LastSeenAt:      time.Unix(int64(e.LastSeen), 0).UTC(),
			OccurrenceCount: int64(e.OccurrenceCount),
			AgentRowID:      int64(e.ID),
			Backtrace:       frames,
		}); err != nil {
			return highest, err
		}
		if id := int64(e.ID); id > highest {
			highest = id
		}
	}
	return highest, nil
}

// flexInt64 unmarshals a JSON value that may arrive as a number or a quoted
// numeric string (wpdb ARRAY_A always encodes numeric columns as strings).
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	// Fast path: plain JSON number.
	if len(b) > 0 && b[0] != '"' {
		var n int64
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		*f = flexInt64(n)
		return nil
	}
	// Quoted string path.
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = flexInt64(n)
	return nil
}

// flexInt is like flexInt64 but for int-sized fields (code, line).
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] != '"' {
		var n int
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		*f = flexInt(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

// ListErrors passes through to the repo. Returns the page and the next-page
// cursor (empty when exhausted).
func (s *Service) ListErrors(ctx context.Context, tenantID, siteID uuid.UUID, f ListPHPErrorsFilter) ([]PHPError, string, error) {
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

// ingestSiteTimezone extracts 'timezone' (IANA string) and 'gmt_offset'
// (float) from the identity category payload and persists them onto the site
// row. It is intentionally best-effort: a missing, null, or malformed field
// is silently skipped (the column defaults stay intact).
func (s *Service) ingestSiteTimezone(ctx context.Context, tenantID, siteID uuid.UUID, payload json.RawMessage) {
	// Decode only the two fields we care about; extra fields are ignored.
	var identity struct {
		Timezone  string  `json:"timezone"`
		GMTOffset float64 `json:"gmt_offset"`
	}
	if err := json.Unmarshal(payload, &identity); err != nil {
		// Malformed payload — skip silently.
		return
	}
	// Nothing to update: both fields are at their zero values.
	if identity.Timezone == "" && identity.GMTOffset == 0 {
		return
	}
	// Non-fatal: log nothing here (the caller controls observability); the
	// diagnostics ingest already succeeded at this point.
	_ = s.repo.UpdateSiteTimezone(ctx, tenantID, siteID, identity.Timezone, identity.GMTOffset)
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
