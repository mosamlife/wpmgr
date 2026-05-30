package diagnostics

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

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
// Errors propagate verbatim — the handler maps the agentcmd "rejected by
// agent: status NNN" error into a 502/503-shaped response, and the unwired
// sentinel is no longer reachable once main wires this impl in.
func (r *RefreshEnqueuerImpl) EnqueueRefreshDiagnostics(ctx context.Context, tenantID, siteID uuid.UUID) error {
	siteURL, err := r.sites.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return fmt.Errorf("resolve site url: %w", err)
	}
	body, err := r.cmd.Diagnostics(ctx, siteID, siteURL, agentcmd.DiagnosticsRequest{})
	if err != nil {
		return err
	}
	// IngestDiagnostics treats an unparseable body as an error so it does not
	// silently drop a malformed response. Per-category upserts that touch the
	// repo are RLS-scoped via the tenantID argument.
	if _, ierr := r.sink.IngestDiagnostics(ctx, tenantID, siteID, body); ierr != nil {
		return fmt.Errorf("ingest agent response: %w", ierr)
	}
	return nil
}

// Compile-time interface checks: this impl MUST satisfy the service-side
// RefreshEnqueuer contract; the agentcmd.Client MUST satisfy the local
// AgentDiagnosticsClient subset. Either check failing surfaces at build time
// rather than at first refresh attempt.
var (
	_ RefreshEnqueuer        = (*RefreshEnqueuerImpl)(nil)
	_ AgentDiagnosticsClient = (*agentcmd.Client)(nil)
)
