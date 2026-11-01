package diagnostics

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// refreshCommand is the agent command the refresh sends, as logged.
const refreshCommand = "diagnostics"

// refreshAction names what did not happen in the operator copy of a failed
// refresh ("Diagnostics check not started. ...").
const refreshAction = "Diagnostics check"

// AgentDiagnosticsClient is the subset of agentcmd.Client the enqueuer needs.
// Declared as an interface so tests can substitute a fake without spinning up
// the SSRF transport. *agentcmd.Client satisfies it via its Diagnostics method
// (added alongside this file).
type AgentDiagnosticsClient interface {
	Diagnostics(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.DiagnosticsRequest) ([]byte, error)
}

// SiteLookup resolves the agent URL for a given (tenant, site). The CP keeps
// the canonical URL in the sites table (RLS-scoped); this interface keeps the
// diagnostics package out of a hard dependency on the site service. The
// adapter in cmd/wpmgr wires *site.Service into it.
type SiteLookup interface {
	GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, error)
}

// IngestSink is the subset of *Service used to feed the raw 14-category blob
// back into the per-category upsert path. Provided as an interface for two
// reasons: (1) the enqueuer is created BEFORE the service value is fully
// configured in main; (2) tests can capture the ingested bytes without round-
// tripping through Postgres.
type IngestSink interface {
	IngestDiagnostics(ctx context.Context, tenantID, siteID uuid.UUID, body []byte) (int, error)
	// ResolveHostProvider runs the M28 host inference. On this refresh (CP->agent
	// pull) path there is no observed egress IP, so it relies on the agent-
	// reported hosting.public_ip carried in the body (0.12.5+ agents).
	ResolveHostProvider(ctx context.Context, tenantID, siteID uuid.UUID, observedIP string, body []byte)
}

// RefreshEnqueuerImpl satisfies diagnostics.RefreshEnqueuer (defined in
// service.go) by dispatching a signed CP->agent `diagnostics` command, reading
// the agent's synchronous 14-category response, and feeding it into the same
// IngestDiagnostics path the daily /agent/v1/diagnostics push uses. The
// trade-off vs an async River job:
//
//   - operator-paced: the "Re-run check" button is a single human click, so
//     blocking the HTTP request on one signed agent POST is fine; the data
//     lands BEFORE the 202 returns, and the UI can refetch immediately.
//   - no River queue dependency: V1 ships without a River-backed enqueuer for
//     diagnostics. The existing agentcmd transport already has the SSRF cap,
//     the per-attempt timeout, and the jti single-use guarantee.
//
// A future revision can swap this for a River-backed enqueuer (mirror update's
// RefreshInventoryWorker) without changing the RefreshEnqueuer interface in
// service.go — that interface is intentionally (tenantID, siteID)-only.
type RefreshEnqueuerImpl struct {
	cmd   AgentDiagnosticsClient
	sites SiteLookup
	sink  IngestSink
	// logger is nil in production, which means slog.Default(); tests set it to
	// capture output.
	logger *slog.Logger
}

// NewRefreshEnqueuer builds a RefreshEnqueuerImpl. All deps are required;
// passing nil is a programmer error (the constructor returns nil to make the
// nil-deref obvious during boot rather than at first call).
func NewRefreshEnqueuer(cmd AgentDiagnosticsClient, sites SiteLookup, sink IngestSink) *RefreshEnqueuerImpl {
	if cmd == nil || sites == nil || sink == nil {
		return nil
	}
	return &RefreshEnqueuerImpl{cmd: cmd, sites: sites, sink: sink}
}

// EnqueueRefreshDiagnostics dispatches one signed `diagnostics` command to the
// site's agent, ingests the response into the per-category store, and returns.
// "Enqueue" in the name preserves the original V1 contract (which envisioned a
// River-backed enqueuer); the body is synchronous because the operator's
// "Re-run check" UX wants the data visible on the next GET.
//
// A failure of the signed call, and a reply that is not the agent's JSON, are
// the site's doing. They come back as a domain.BadGateway (HTTP 502) carrying a
// stable code and fixed operator copy (agentcmd.DescribeSiteFailure), after one
// warn log line. Neither the site's reply nor err.Error() reaches the response
// or the log. A domain error keeps its kind: the 404 for an unknown site, the
// 422 for a reply without collected_at. Anything else, such as a database
// failure while storing the report, comes back as it is, and the handler
// answers it with a 500.
func (r *RefreshEnqueuerImpl) EnqueueRefreshDiagnostics(ctx context.Context, tenantID, siteID uuid.UUID) error {
	siteURL, err := r.sites.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return fmt.Errorf("resolve site url: %w", err)
	}
	body, err := r.cmd.Diagnostics(ctx, siteID, siteURL, agentcmd.DiagnosticsRequest{})
	if err != nil {
		return r.siteFailure(ctx, tenantID, siteID, err)
	}
	// IngestDiagnostics treats an unparseable body as an error so it does not
	// silently drop a malformed response. Per-category upserts that touch the
	// repo are RLS-scoped via the tenantID argument.
	//
	// A reply without a valid collected_at is refused with a validation error
	// and nothing is stored (GH #618). That refusal is final for this click:
	// the refresh makes no second agent call, skips host inference below, and
	// the wrapped error keeps its domain kind, so the operator gets a 422
	// naming the code rather than a 500.
	//
	// A reply that is not a JSON object fails to decode before any of that. It
	// is not a domain error, and it is the site's doing, so it is answered like
	// any other unusable reply from the site. A failure to store the report is
	// neither: only a decode error is taken for the site's.
	if _, ierr := r.sink.IngestDiagnostics(ctx, tenantID, siteID, body); ierr != nil {
		if _, isDomain := domain.AsDomain(ierr); !isDomain && agentcmd.IsDecodeErr(ierr) {
			return r.siteFailure(ctx, tenantID, siteID, ierr)
		}
		return fmt.Errorf("ingest agent response: %w", ierr)
	}
	// M28 — this is a CP->agent pull, so there is no observed egress IP; resolve
	// the host from the agent-reported hosting.public_ip in the body (0.12.5+).
	// Best-effort and non-fatal: a 0.12.4 agent (no public_ip) simply leaves the
	// inferred host unchanged.
	r.sink.ResolveHostProvider(ctx, tenantID, siteID, "", body)
	return nil
}

// siteFailure turns err into the 502 the operator sees when err is the site's
// doing, and logs it once. An err that is not (agentcmd.DescribeSiteFailure
// says so) is returned as it came.
//
// The log line names the site, the command, the HTTP status when the site gave
// one, and the code. It never carries err: for a rejected command its text
// includes a snippet of the site's reply.
func (r *RefreshEnqueuerImpl) siteFailure(ctx context.Context, tenantID, siteID uuid.UUID, err error) error {
	f, ok := agentcmd.DescribeSiteFailure(err, refreshAction)
	if !ok {
		return err
	}
	attrs := []any{
		slog.String("tenant_id", tenantID.String()),
		slog.String("site_id", siteID.String()),
		slog.String("command", refreshCommand),
		slog.String("code", f.Code),
	}
	if f.Status != 0 {
		attrs = append(attrs, slog.Int("http_status", f.Status))
	}
	r.log().WarnContext(ctx, "diagnostics refresh: the site did not give a usable answer", attrs...)
	return domain.BadGateway(f.Code, f.Message)
}

// log returns the enqueuer's logger, or the process default when none is set.
func (r *RefreshEnqueuerImpl) log() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return slog.Default()
}

// Compile-time interface checks: this impl MUST satisfy the service-side
// RefreshEnqueuer contract; the agentcmd.Client MUST satisfy the local
// AgentDiagnosticsClient subset. Either check failing surfaces at build time
// rather than at first refresh attempt.
var (
	_ RefreshEnqueuer        = (*RefreshEnqueuerImpl)(nil)
	_ AgentDiagnosticsClient = (*agentcmd.Client)(nil)
)
