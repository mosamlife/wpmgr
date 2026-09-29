package site

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/siteaddr"
)

const (
	// adoptURLJobTimeout bounds one adoption job: the site load, at most one
	// signed probe, and the write.
	adoptURLJobTimeout = 30 * time.Second
	// adoptURLUniqueWindow is how long one (site, reported address) job
	// stands in for every later push of the same address, so a site that
	// pushes often queues at most one job per window.
	adoptURLUniqueWindow = time.Hour
	// adoptURLMaxAttempts retries a load or write failure; a refusal is not
	// a failure and is never retried.
	adoptURLMaxAttempts = 3
)

// AdoptReportedURLArgs is the River job that runs AdoptReportedURL for an
// address an agent push reported. The push only enqueues it, so the signed
// probe that confirms a change never holds up the push.
//
// SiteID and ReportedKey carry `river:"unique"`: with ByArgs, the uniqueness
// key is those two fields only, so repeated pushes of one address by one site
// inside adoptURLUniqueWindow insert one job, however the address is spelt.
type AdoptReportedURLArgs struct {
	TenantID uuid.UUID `json:"tenant_id"`
	SiteID   uuid.UUID `json:"site_id" river:"unique"`
	// Reported is the address as the agent reported it, trimmed. The job
	// decides from it and its audit row records it.
	Reported string `json:"reported"`
	// ReportedKey is Reported's canonical form (reportedAddressKey), which
	// the job is unique by and nothing else reads.
	ReportedKey  string `json:"reported_key" river:"unique"`
	Source       string `json:"source"`
	AgentVersion string `json:"agent_version,omitempty"`
}

// Kind implements river.JobArgs. It must stay stable: changing it orphans
// queued jobs.
func (AdoptReportedURLArgs) Kind() string { return "site_adopt_reported_url" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (AdoptReportedURLArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		MaxAttempts: adoptURLMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: adoptURLUniqueWindow,
		},
	}
}

// AdoptURLEnqueuer inserts an AdoptReportedURLArgs job.
// RiverAdoptURLEnqueuer implements it.
type AdoptURLEnqueuer interface {
	EnqueueAdoptURL(ctx context.Context, args AdoptReportedURLArgs) error
}

// RiverAdoptURLEnqueuer inserts adoption jobs through a River client.
type RiverAdoptURLEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverAdoptURLEnqueuer builds the enqueuer around a River client.
func NewRiverAdoptURLEnqueuer(client *river.Client[pgx.Tx]) *RiverAdoptURLEnqueuer {
	return &RiverAdoptURLEnqueuer{client: client}
}

// EnqueueAdoptURL inserts the job with its own InsertOpts. A duplicate inside
// the unique window is skipped by River and is not an error.
func (e *RiverAdoptURLEnqueuer) EnqueueAdoptURL(ctx context.Context, args AdoptReportedURLArgs) error {
	if _, err := e.client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("enqueue site address adoption: %w", err)
	}
	return nil
}

// SetAdoptURLEnqueuer wires the queue that agent pushes hand a reported
// address to. Without one, a reported address is not adopted after
// enrollment.
func (s *Service) SetAdoptURLEnqueuer(e AdoptURLEnqueuer) { s.adoptQueue = e }

const (
	// maxReportedURLLen is the longest reported address, in bytes, that a
	// push may queue. A longer one is dropped before it reaches the job
	// table.
	maxReportedURLLen = 2048
	// maxReportedKeyLen is the longest canonical form (reportedAddressKey),
	// in bytes, that a push may queue. Escaping writes a path byte as at
	// most three, so no report under maxReportedURLLen with an ASCII host
	// exceeds it; a longer one is dropped like an oversized report.
	maxReportedKeyLen = 3 * maxReportedURLLen
)

// reportedAddressKey is the canonical form of a reported site address that
// the adoption job is unique by: the scheme, the host's siteaddr.HostKey, the
// port with the scheme's default dropped, and the path through
// siteaddr.NormalizePath, escaped. Spellings of one address that differ only
// in the case of the scheme or an ASCII host, a written default port, a
// trailing slash or the escaping of the path share one key. ok is false for
// an address siteaddr.Parse refuses or whose host has no HostKey, which
// PlanStrict never adopts.
func reportedAddressKey(reported string) (string, bool) {
	a, u, ok := parseSiteAddress(reported)
	if !ok {
		return "", false
	}
	host, ok := siteaddr.HostKey(u.Hostname())
	if !ok {
		return "", false
	}
	path := (&url.URL{Path: a.Path}).EscapedPath()
	return siteaddr.Join(a.Scheme, host, a.Port, path), true
}

// EnqueueAdoptReportedURL queues AdoptReportedURL for an address an agent
// push reported, and returns without probing the site. It satisfies
// diagnostics.ReportedURLSink. The error, when the insert fails, is logged
// here and returned for the caller to drop: a push is never failed by it.
//
// A report longer than maxReportedURLLen bytes, one that is not a plain
// http(s) site address (siteaddr.Parse) or whose host has no HostKey, or one
// whose canonical form is longer than maxReportedKeyLen bytes, is dropped
// with a Debug log and queues nothing: the job table is shared, and a report
// the job could never adopt has no business in it. The agent version is cut
// to maxAgentVersion runes, the limit the stored copy is held to.
func (s *Service) EnqueueAdoptReportedURL(ctx context.Context, tenantID, siteID uuid.UUID, reported, source, agentVersion string) error {
	return s.enqueueAdoptReportedURL(ctx, tenantID, siteID, "", reported, source, agentVersion)
}

// enqueueAdoptReportedURL is EnqueueAdoptReportedURL. saved, when not empty,
// is the site's saved address as the push read it, and a report that the
// job's rule (planReportedURL) would not adopt over it is dropped too. The
// metadata push passes it from the row it has just written; the diagnostics
// push has no site row to hand and passes "".
func (s *Service) enqueueAdoptReportedURL(ctx context.Context, tenantID, siteID uuid.UUID, saved, reported, source, agentVersion string) error {
	reported = strings.TrimSpace(reported)
	if reported == "" {
		return nil
	}
	log := s.logger.With(slog.String("site_id", siteID.String()), slog.String("source", source))
	if len(reported) > maxReportedURLLen {
		log.Debug("adopt reported address: not queued: longer than the address limit",
			slog.Int("bytes", len(reported)), slog.Int("limit", maxReportedURLLen))
		return nil
	}
	key, ok := reportedAddressKey(reported)
	if !ok {
		log.Debug("adopt reported address: not queued: not a site address",
			slog.String("reported", sanitizeReportedURL(reported)))
		return nil
	}
	if len(key) > maxReportedKeyLen {
		log.Debug("adopt reported address: not queued: canonical form longer than its limit",
			slog.Int("bytes", len(key)), slog.Int("limit", maxReportedKeyLen))
		return nil
	}
	if saved != "" && planReportedURL(saved, reported).Decision != enrollURLAdopt {
		log.Debug("adopt reported address: not queued: not an address the saved one could become",
			slog.String("saved", saved), slog.String("reported", sanitizeReportedURL(reported)))
		return nil
	}
	if s.adoptQueue == nil {
		log.Debug("adopt reported address: not queued: no queue is wired")
		return nil
	}
	err := s.adoptQueue.EnqueueAdoptURL(ctx, AdoptReportedURLArgs{
		TenantID:     tenantID,
		SiteID:       siteID,
		Reported:     reported,
		ReportedKey:  key,
		Source:       source,
		AgentVersion: truncateRunes(agentVersion, maxAgentVersion),
	})
	if err != nil {
		log.Warn("adopt reported address: enqueue failed", slog.Any("error", err))
	}
	return err
}

// AdoptReportedURLWorker runs AdoptReportedURLArgs jobs.
type AdoptReportedURLWorker struct {
	river.WorkerDefaults[AdoptReportedURLArgs]
	svc *Service
}

// NewAdoptReportedURLWorker builds the worker around the site service.
func NewAdoptReportedURLWorker(svc *Service) *AdoptReportedURLWorker {
	return &AdoptReportedURLWorker{svc: svc}
}

// Timeout implements river.Worker.
func (w *AdoptReportedURLWorker) Timeout(*river.Job[AdoptReportedURLArgs]) time.Duration {
	return adoptURLJobTimeout
}

// Work runs AdoptReportedURL for the job's address. A load or write failure is
// returned so River retries it; a site that no longer exists is done.
func (w *AdoptReportedURLWorker) Work(ctx context.Context, job *river.Job[AdoptReportedURLArgs]) error {
	ctx, cancel := context.WithTimeout(ctx, adoptURLJobTimeout)
	defer cancel()
	a := job.Args
	err := w.svc.AdoptReportedURL(ctx, a.TenantID, a.SiteID, a.Reported, a.Source, a.AgentVersion)
	if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
		return nil
	}
	return err
}
