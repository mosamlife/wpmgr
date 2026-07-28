package site

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	agentpkg "github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// Service holds site business logic. All operations require a tenant ID, which
// the handler derives from request context (tenant middleware) — except the
// enrollment and agent paths, which resolve the tenant from a pairing code or
// the agent's verified identity respectively.
type Service struct {
	repo      Repo
	validator *domain.Validator
	clock     domain.Clock
	logger    *slog.Logger
	// conn is the M21 connection-lifecycle service. Optional: when wired, a
	// site-bound enrollment code transitions the bound site (the live-enroll
	// flow); a legacy NULL-site_id code keeps the create-at-enroll path. nil ⇒
	// every code uses the legacy path (back-compat).
	conn ConnectionService
	// uptimeStore is the metrics store used to enrich site-list rows with uptime
	// fields. When set, List() calls QueryFleetUptime (one batch query) instead
	// of reading site_uptime_probes directly — this ensures ClickHouse
	// deployments see real data (ClickHouse never writes to site_uptime_probes).
	// nil disables uptime enrichment (tests that don't inject a store).
	uptimeStore metrics.Store
}

// NewService builds a site Service.
func NewService(repo Repo, v *domain.Validator, clock domain.Clock) *Service {
	return &Service{repo: repo, validator: v, clock: clock, logger: slog.Default()}
}

// SetLogger wires a structured logger for slow-request diagnostics. Called once
// at boot; defaults to slog.Default() when not called (safe for tests).
func (s *Service) SetLogger(l *slog.Logger) {
	if l != nil {
		s.logger = l
	}
}

// SetUptimeStore wires the metrics store for uptime enrichment in List().
// Call once at boot after the store is constructed. No-op when store is nil.
// The store is transparently wrapped in a short-TTL in-process cache
// (fleetUptimeCacheTTL = 60s) so repeated /sites loads within the TTL window
// skip the QueryFleetUptime round-trip entirely (m85 perf fix).
func (s *Service) SetUptimeStore(store metrics.Store) {
	if store == nil {
		return
	}
	s.uptimeStore = newCachedMetricsStore(store)
}

// SetConnectionService wires the M21 lifecycle service into the enroll branch.
// Call once at boot (the lifecycle service depends on this Service's repo, so
// it is constructed after and injected here).
func (s *Service) SetConnectionService(cs ConnectionService) { s.conn = cs }

// SetScreenshotEnricher wires the M72 screenshot enricher onto THIS service's
// underlying repo, so List() populates screenshot_status + presigned URLs. It
// MUST be used instead of the free SetScreenshotEnricher(repo, e) at boot: the
// list path is served by this service's own repo instance, and wiring the
// enricher onto any other repo instance is a silent no-op (the enrichment never
// runs and every card falls back to the "never" placeholder). No-op if the repo
// does not support enrichment.
func (s *Service) SetScreenshotEnricher(e ScreenshotEnricher) { SetScreenshotEnricher(s.repo, e) }

// SetBillingGate wires the M16 Phase A billing gate onto THIS service's
// underlying repo, so Create() (the legacy/back-compat direct-create path)
// enforces the hosted-plan site cap. Mirrors SetScreenshotEnricher's wiring
// rule exactly: cmd/wpmgr must ALSO call the free site.SetBillingGate on the
// separate repo instance handed to NewConnectionService, or the
// CreatePending/Restore paths stay uncapped — see BillingGate's doc comment
// in repo.go. No-op if the repo does not support the gate.
func (s *Service) SetBillingGate(b BillingGate) { SetBillingGate(s.repo, b) }

// Create validates and persists a new site under the given tenant.
func (s *Service) Create(ctx context.Context, in CreateInput) (Site, error) {
	if in.TenantID == uuid.Nil {
		return Site{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	if err := s.validator.Struct(in); err != nil {
		return Site{}, err
	}
	return s.repo.Create(ctx, in)
}

// Get returns a tenant-scoped site by ID.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Site, error) {
	if tenantID == uuid.Nil {
		return Site{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	return s.repo.Get(ctx, tenantID, id)
}

// slowListThreshold is the per-phase latency above which List emits a WARN
// log with per-phase timing. At 200 ms this fires on the symptom (post-idle
// ~8 s stall) while staying silent on normal fast requests (~50 ms). A single
// structured log line with repo_ms, uptime_ms, and total_ms pinpoints which
// phase is slow without any tracing infrastructure.
const slowListThreshold = 200 * time.Millisecond

// List returns a page of the tenant's sites, optionally filtered by tag.
// Uptime fields (UptimeUp, UptimePct30d, AvgLatencyMs, TLSExpiresAt) are
// enriched via the metrics.Store when one is wired, so both ClickHouse and
// Postgres deployments populate them. Without a wired store, the fields remain
// nil (unprobed). The 30-day window matches the previous listLatestUptimeSQL.
//
// Phase timing: when total elapsed exceeds slowListThreshold, a WARN log is
// emitted with repo_ms (DB tx: acquire + queries + enrichment), uptime_ms
// (QueryFleetUptime), and total_ms. This isolates a DB dead-connection acquire
// stall (slow repo_ms, fast uptime_ms) from a slow uptime query or a slow
// Redis session load (not visible here — that phase runs in middleware).
func (s *Service) List(ctx context.Context, in ListInput) ([]Site, error) {
	if in.TenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	in.Limit, in.Offset = normalizePage(in.Limit, in.Offset)

	t0 := time.Now()

	sites, err := s.repo.List(ctx, in)
	repoElapsed := time.Since(t0)

	if err != nil {
		return nil, err
	}

	var uptimeElapsed time.Duration
	if len(sites) > 0 && s.uptimeStore != nil {
		ids := make([]uuid.UUID, len(sites))
		for i := range sites {
			ids[i] = sites[i].ID
		}
		const window30d = 30 * 24 * time.Hour
		tu := time.Now()
		uptimeMap, uerr := s.uptimeStore.QueryFleetUptime(ctx, in.TenantID, ids, window30d)
		uptimeElapsed = time.Since(tu)
		if uerr != nil {
			// Non-fatal: log and continue — the site list is still usable without
			// uptime data (the same tolerance as the old screenshot enrichment path).
			_ = uerr
		} else {
			for i := range sites {
				um, ok := uptimeMap[sites[i].ID]
				if !ok {
					continue
				}
				sites[i].UptimeUp = um.Up
				sites[i].TLSExpiresAt = um.TLSExpiry
				sites[i].AvgLatencyMs = um.AvgLatencyMs
				if um.UptimePct7d != nil {
					sites[i].UptimePct30d = um.UptimePct7d
				}
			}
		}
	}

	totalElapsed := time.Since(t0)
	if totalElapsed >= slowListThreshold {
		// WARN on any request that took longer than the threshold so post-idle
		// stalls are immediately visible in Cloud Logging without noise on the
		// fast path. repo_ms covers the full InTenantTx (pool acquire + GUC set
		// + ListSites + backup + client + screenshot enrichment + commit).
		// uptime_ms covers QueryFleetUptime (a second InAgentTx).
		// If the 8 s is in the DB dead-conn path, repo_ms will dominate.
		// If repo_ms is fast and uptime_ms dominates, the uptime query is slow.
		// If both are fast, the delay is outside this service (e.g. Redis session
		// load in the auth middleware — check the overall request latency vs total_ms).
		s.logger.WarnContext(ctx, "site list slow",
			slog.Int64("repo_ms", repoElapsed.Milliseconds()),
			slog.Int64("uptime_ms", uptimeElapsed.Milliseconds()),
			slog.Int64("total_ms", totalElapsed.Milliseconds()),
			slog.Int("site_count", len(sites)),
		)
	}

	return sites, nil
}

// ListAllSiteIDs returns every non-archived site ID for the tenant in a single
// lightweight query (no row cap). Use this for fleet enumeration.
func (s *Service) ListAllSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	if tenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	return s.repo.ListAllSiteIDs(ctx, tenantID)
}

// Delete removes a tenant-scoped site.
func (s *Service) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil {
		return domain.Forbidden("tenant_required", "a tenant context is required")
	}
	return s.repo.Delete(ctx, tenantID, id)
}

// SetTags replaces the tag set on a tenant-scoped site (deduplicated, trimmed).
func (s *Service) SetTags(ctx context.Context, in SetTagsInput) (Site, error) {
	if in.TenantID == uuid.Nil {
		return Site{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	in.Tags = normalizeTags(in.Tags)
	if err := s.validator.Struct(in); err != nil {
		return Site{}, err
	}
	return s.repo.SetTags(ctx, in)
}

// SetAgeRecipient stores the per-site age PUBLIC recipient that backups for the
// site are encrypted to (client-side, on the agent). The control plane never
// holds the matching identity, so it cannot decrypt backups.
func (s *Service) SetAgeRecipient(ctx context.Context, tenantID, siteID uuid.UUID, recipient string) (Site, error) {
	if tenantID == uuid.Nil {
		return Site{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	return s.repo.SetAgeRecipient(ctx, tenantID, siteID, recipient)
}

// CreatePairingCode generates a one-time, short-TTL pairing code for the tenant
// and returns the plaintext (shown once) plus the stored record.
func (s *Service) CreatePairingCode(ctx context.Context, in CreatePairingCodeInput) (CreatedPairingCode, error) {
	if in.TenantID == uuid.Nil {
		return CreatedPairingCode{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	in.Tags = normalizeTags(in.Tags)
	if err := s.validator.Struct(in); err != nil {
		return CreatedPairingCode{}, err
	}
	plaintext, err := generatePairingCode()
	if err != nil {
		return CreatedPairingCode{}, domain.Internal("pairing_code_gen_failed", "failed to generate pairing code").WithCause(err)
	}
	expiresAt := s.clock.Now().Add(pairingCodeTTL)
	pc, err := s.repo.CreatePairingCode(ctx, in, hashPairingCode(plaintext), expiresAt)
	if err != nil {
		return CreatedPairingCode{}, err
	}
	return CreatedPairingCode{Code: pc, Plaintext: plaintext}, nil
}

// EnrollRequest is the validated public /enroll input.
type EnrollRequest struct {
	PairingCode    string `validate:"required,max=128"`
	SiteURL        string `validate:"required,url,max=2048"`
	AgentPublicKey string `validate:"required,base64"`
	Name           string `validate:"max=200"`
	WPVersion      string `validate:"max=32"`
	PHPVersion     string `validate:"max=32"`
	// m100 follow-up (GH #230 security review): same cap as
	// CreatePairingCodeInput.Tags/MintEnrollmentInput.Tags — previously
	// unbounded, letting an agent-supplied tag exceed site_tags'
	// char_length<=64 CHECK once registered. Enroll's s.validator.Struct(req)
	// call below is the FIRST statement in Enroll (before any write), so this
	// rejects an over-length tag before the site is created/attached.
	Tags []string `validate:"max=50,dive,min=1,max=64"`
	// ConsumedFromIP is the agent's source IP (best-effort, from the request).
	// Recorded on a site-bound consume for audit; ignored on the legacy path.
	ConsumedFromIP string `validate:"-"`
}

// Enroll validates an enroll request, verifies the agent public key is a
// well-formed Ed25519 key, then resolves+consumes the code and creates/attaches
// the site (rotating the agent key on re-enrollment). The tenant is derived
// entirely from the pairing code — never from the caller.
func (s *Service) Enroll(ctx context.Context, req EnrollRequest) (Site, error) {
	if err := s.validator.Struct(req); err != nil {
		return Site{}, err
	}
	// Reject site URLs whose scheme isn't http/https (the SSRF transport blocks
	// non-http(s) at dial anyway, but rejecting at enrollment avoids storing
	// file:// / gopher:// / javascript: garbage in the registry).
	if u, err := url.Parse(req.SiteURL); err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") {
		return Site{}, domain.Validation("site_url_scheme", "site_url must be an http or https URL")
	}
	// Reject a syntactically valid base64 that is not a 32-byte Ed25519 key.
	if _, err := agentpkg.DecodePublicKey(req.AgentPublicKey); err != nil {
		return Site{}, domain.Validation("agent_public_key_invalid", "agent_public_key is not a valid Ed25519 public key")
	}
	codeHash := hashPairingCode(req.PairingCode)

	// M21: route between the site-first consume (code bound to an existing
	// pending_enrollment site) and the legacy create-at-enroll flow. The peek
	// does not consume; an unknown code surfaces the same invalid-code error the
	// legacy path returns. When the lifecycle service is not wired, every code
	// takes the legacy path (full back-compat).
	if s.conn != nil {
		if _, bound, perr := s.repo.PairingCodeSiteID(ctx, codeHash); perr != nil {
			return Site{}, perr
		} else if bound {
			return s.conn.ConsumeEnrollmentCode(ctx, ConsumeEnrollmentInput{
				CodeHash:       codeHash,
				AgentPublicKey: req.AgentPublicKey,
				SiteURL:        req.SiteURL,
				ConsumedFromIP: req.ConsumedFromIP,
				Meta:           Metadata{WPVersion: req.WPVersion, PHPVersion: req.PHPVersion},
			})
		}
	}

	return s.repo.Enroll(ctx, codeHash, EnrollInput{
		URL:            req.SiteURL,
		Name:           req.Name,
		AgentPublicKey: req.AgentPublicKey,
		WPVersion:      req.WPVersion,
		PHPVersion:     req.PHPVersion,
		Tags:           normalizeTags(req.Tags),
	})
}

// ResolveByAgentKey resolves an enrolled site by its agent public key,
// satisfying agent.SiteResolver. The returned identity drives the agent-auth
// middleware (site + tenant come from the verified key).
func (s *Service) ResolveByAgentKey(ctx context.Context, agentPublicKey string) (agentpkg.Identity, error) {
	site, err := s.repo.GetByAgentKey(ctx, agentPublicKey)
	if err != nil {
		return agentpkg.Identity{}, err
	}
	return agentpkg.Identity{SiteID: site.ID, TenantID: site.TenantID}, nil
}

// RecordNonce records an anti-replay nonce for a site (agent.SiteResolver).
func (s *Service) RecordNonce(ctx context.Context, siteID uuid.UUID, nonce string) (bool, error) {
	return s.repo.RecordNonce(ctx, siteID, nonce)
}

// ApplyAgentMetadata adapts agent-package metadata to the site domain and
// returns the updated site in OpenAPI form, satisfying agent.MetadataSink.
func (s *Service) ApplyAgentMetadata(ctx context.Context, tenantID, siteID uuid.UUID, m agentpkg.Metadata) (gen.Site, error) {
	out, err := s.ApplyMetadata(ctx, tenantID, siteID, Metadata{
		WPVersion:    m.WPVersion,
		PHPVersion:   m.PHPVersion,
		ServerInfo:   m.ServerInfo,
		Multisite:    m.Multisite,
		ActiveTheme:  m.ActiveTheme,
		AgentVersion: m.AgentVersion,
		Plugins:      fromAgentComponents(m.Plugins),
		Themes:       fromAgentComponents(m.Themes),
		CoreUpdate:   fromAgentCoreUpdate(m.CoreUpdate),
		// ADR-037 Sprint 1, 1C — sparse-metadata expansion. The new fields are
		// stored inside the existing JSONB `components` column (no migration);
		// the strict ogen Site type round-trips them as raw JSON inside the
		// components blob. UI reads via the components.host_flags /
		// components.disk / components.user_count / components.admin_count
		// path. Old agents send none of these and the fields stay absent.
		Extras: fromAgentMetadataExtras(m),
		// The agent's own account of its last self-update apply beat. Additive
		// and tolerant of absence: old agents send nothing here.
		AgentSelfUpdate: fromAgentSelfUpdateResult(m.AgentSelfUpdate),
	})
	if err != nil {
		return gen.Site{}, err
	}
	// Opportunistically register the agent's age recipient (M4 backups need it).
	// Best-effort: a malformed recipient is silently ignored — the agent will
	// retry on the next sync, and operators can also set it explicitly elsewhere.
	if rec := strings.TrimSpace(m.AgeRecipient); rec != "" && len(rec) <= 256 &&
		strings.HasPrefix(rec, "age1") && out.AgeRecipient != rec {
		if updated, err := s.repo.SetAgeRecipient(ctx, tenantID, siteID, rec); err == nil {
			out = updated
		}
	}
	return toAPI(out), nil
}

func fromAgentComponents(cs []agentpkg.Component) []Component {
	out := make([]Component, 0, len(cs))
	for _, c := range cs {
		comp := Component{
			Slug:      c.Slug,
			Name:      c.Name,
			Version:   c.Version,
			Active:    c.Active,
			PluginURI: c.PluginURI,
			UpdateURI: c.UpdateURI,
			AuthorURI: c.AuthorURI,
			Network:   c.Network,
		}
		if c.AvailableUpdate != nil && c.AvailableUpdate.NewVersion != "" {
			comp.AvailableUpdate = &AvailableUpdate{
				NewVersion:  c.AvailableUpdate.NewVersion,
				Package:     c.AvailableUpdate.Package,
				Tested:      c.AvailableUpdate.Tested,
				RequiresPHP: c.AvailableUpdate.RequiresPHP,
			}
		}
		out = append(out, comp)
	}
	return out
}

// fromAgentMetadataExtras lifts the optional sparse-metadata expansion fields
// (host_flags / disk / user_count / admin_count) from the agent.Metadata DTO
// onto the site domain's MetadataExtras struct. Returns nil when the agent
// sent nothing (old agent; the sink does not overwrite previously-stored
// values in that case — see ApplyMetadata).
func fromAgentMetadataExtras(m agentpkg.Metadata) *MetadataExtras {
	if m.HostFlags == nil && m.Disk == nil && m.UserCount == 0 && m.AdminCount == 0 {
		return nil
	}
	x := &MetadataExtras{
		UserCount:  m.UserCount,
		AdminCount: m.AdminCount,
	}
	if m.HostFlags != nil {
		x.HostFlags = &HostFlags{
			IsPressable: m.HostFlags.IsPressable,
			IsGridpane:  m.HostFlags.IsGridpane,
			IsWPEngine:  m.HostFlags.IsWPEngine,
			IsAtomic:    m.HostFlags.IsAtomic,
			IsKinsta:    m.HostFlags.IsKinsta,
			IsFlywheel:  m.HostFlags.IsFlywheel,
			IsRunCloud:  m.HostFlags.IsRunCloud,
			IsCloudways: m.HostFlags.IsCloudways,
		}
	}
	if m.Disk != nil {
		x.Disk = &Disk{
			WPContentBytes: m.Disk.WPContentBytes,
			UploadsBytes:   m.Disk.UploadsBytes,
			FreeBytes:      m.Disk.FreeBytes,
		}
	}
	return x
}

// fromAgentSelfUpdateResult lifts the agent's apply-beat record from the agent
// package DTO onto the site domain type. nil in, nil out: an agent that never
// staged a self-update, and every agent predating the channel, sends nothing.
func fromAgentSelfUpdateResult(r *agentpkg.AgentSelfUpdateResult) *AgentSelfUpdateResult {
	if r == nil || r.Status == "" {
		return nil
	}
	return &AgentSelfUpdateResult{
		Status:      r.Status,
		FromVersion: r.FromVersion,
		ToVersion:   r.ToVersion,
		Detail:      r.Detail,
		At:          r.At,
	}
}

func fromAgentCoreUpdate(cu *agentpkg.CoreUpdate) *CoreUpdate {
	if cu == nil || cu.NewVersion == "" {
		return nil
	}
	return &CoreUpdate{NewVersion: cu.NewVersion, CurrentVersion: cu.CurrentVersion}
}

// Metadata sanitization bounds. Metadata is best-effort telemetry from
// arbitrary real-world sites, so we never reject a sync over field lengths, // we truncate (on rune boundaries) and cap slice sizes instead.
const (
	maxWPVersion    = 32
	maxPHPVersion   = 32
	maxServerInfo   = 512
	maxActiveTheme  = 200
	maxComponentLen = 200 // slug + name
	maxVersionLen   = 64
	maxPlugins      = 5000
	maxThemes       = 1000
	// AvailableUpdate field bounds. package_url can be reasonably long, so we
	// allow more headroom; tested/requires_php are short version strings.
	maxPackageURL  = 2048
	maxTestedLen   = 32
	maxRequiresPHP = 32
	// Agent self-update apply-record bounds. The agent already caps its own
	// detail at 500 characters, but that is the agent's promise, not a bound
	// this side may rely on: the record is agent-supplied telemetry like every
	// other field here and is truncated on arrival. maxSelfUpdateStatus is
	// generous enough for a status a newer agent introduces.
	maxSelfUpdateStatus = 32
	maxSelfUpdateDetail = 500
)

// truncateRunes returns s truncated to at most n runes, never splitting a
// multi-byte UTF-8 sequence.
func truncateRunes(s string, n int) string {
	if n < 0 {
		n = 0
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// sanitizeComponents truncates each component's fields, drops components whose
// slug is empty after trimming, and caps the slice length.
func sanitizeComponents(cs []Component, maxLen int) []Component {
	out := make([]Component, 0, len(cs))
	for _, c := range cs {
		slug := strings.TrimSpace(c.Slug)
		if slug == "" {
			continue
		}
		comp := Component{
			Slug:    truncateRunes(slug, maxComponentLen),
			Name:    truncateRunes(c.Name, maxComponentLen),
			Version: truncateRunes(c.Version, maxVersionLen),
			Active:  c.Active,
		}
		// The agent's own plugin is never an actionable update target: applying
		// a plugin update to it is an in-process self-overwrite whose rollback
		// is structurally undeliverable, so the advisory is dropped here (the
		// universal write chokepoint) and the operator can never select it. The
		// COMPONENT itself is kept, with its installed version, so the agent
		// stays visible in the inventory; the agent upgrades itself over the
		// signed ADR-042 channel. The agent refuses the same target on its side
		// as well (defense in depth, not redundancy).
		//
		// Identity is taken from the entry's plugin-header NAME as well as its
		// slug: the slug is the directory the archive happened to be unpacked
		// into (a host uploader or an operator can leave it as
		// "wpmgr-agent-0.61.88"), while the header travels inside the archive.
		// This is the fleet's only guard until every site has self-updated past
		// the release that adds the agent's own refusal, so it must not depend
		// on a folder name we do not control.
		if c.AvailableUpdate != nil && !agentplugin.IsComponent(comp.Slug, comp.Name) {
			nv := strings.TrimSpace(c.AvailableUpdate.NewVersion)
			// GH #211: a WordPress update transient can report new_version ==
			// the already-installed Version (observed with Kadence, e.g.
			// "1.5.1 -> 1.5.1"). This is the universal write chokepoint for
			// agent-pushed metadata, so a same-version advisory is never
			// persisted in the first place. wpversion.SameVersion is
			// EQUALITY-only (never a version_compare-style ordering) and fails
			// open (false) when either side is empty, so a component with an
			// unknown installed Version always keeps its advisory.
			if nv != "" && !wpversion.SameVersion(comp.Version, nv) {
				comp.AvailableUpdate = &AvailableUpdate{
					NewVersion:  truncateRunes(nv, maxVersionLen),
					Package:     truncateRunes(c.AvailableUpdate.Package, maxPackageURL),
					Tested:      truncateRunes(c.AvailableUpdate.Tested, maxTestedLen),
					RequiresPHP: truncateRunes(c.AvailableUpdate.RequiresPHP, maxRequiresPHP),
				}
			}
		}
		out = append(out, comp)
		if len(out) >= maxLen {
			break
		}
	}
	return out
}

// sanitizeCoreUpdate truncates the core-update advisory fields and drops the
// whole advisory when new_version is empty (the contract requires it), or
// when new_version normalizes equal to current_version (GH #211-class
// phantom advisory — see sanitizeComponents).
func sanitizeCoreUpdate(cu *CoreUpdate) *CoreUpdate {
	if cu == nil {
		return nil
	}
	nv := truncateRunes(strings.TrimSpace(cu.NewVersion), maxWPVersion)
	if nv == "" {
		return nil
	}
	cv := truncateRunes(cu.CurrentVersion, maxWPVersion)
	if wpversion.SameVersion(cv, nv) {
		return nil
	}
	return &CoreUpdate{
		NewVersion:     nv,
		CurrentVersion: cv,
	}
}

// sanitizeAgentSelfUpdate bounds the agent's apply-beat record and drops it
// entirely when it carries no status, which is the one field that makes it mean
// anything. Everything else is best-effort and may be empty.
func sanitizeAgentSelfUpdate(r *AgentSelfUpdateResult) *AgentSelfUpdateResult {
	if r == nil {
		return nil
	}
	status := truncateRunes(strings.TrimSpace(r.Status), maxSelfUpdateStatus)
	if status == "" {
		return nil
	}
	at := r.At
	if at < 0 {
		at = 0
	}
	return &AgentSelfUpdateResult{
		Status:      status,
		FromVersion: truncateRunes(strings.TrimSpace(r.FromVersion), maxVersionLen),
		ToVersion:   truncateRunes(strings.TrimSpace(r.ToVersion), maxVersionLen),
		Detail:      truncateRunes(r.Detail, maxSelfUpdateDetail),
		At:          at,
	}
}

// sanitizeMetadata coerces arbitrary agent-reported metadata into the stored
// bounds without ever erroring: scalar fields are truncated on rune
// boundaries, components with empty slugs are dropped, and the plugin/theme
// slices are capped. This is the single source of truth for metadata bounds.
func sanitizeMetadata(m Metadata) Metadata {
	return Metadata{
		WPVersion:    truncateRunes(m.WPVersion, maxWPVersion),
		PHPVersion:   truncateRunes(m.PHPVersion, maxPHPVersion),
		ServerInfo:   truncateRunes(m.ServerInfo, maxServerInfo),
		Multisite:    m.Multisite,
		ActiveTheme:  truncateRunes(m.ActiveTheme, maxActiveTheme),
		AgentVersion: truncateRunes(m.AgentVersion, 64),
		Plugins:      sanitizeComponents(m.Plugins, maxPlugins),
		Themes:       sanitizeComponents(m.Themes, maxThemes),
		CoreUpdate:   sanitizeCoreUpdate(m.CoreUpdate),
		// ADR-037 Sprint 1, 1C — Extras pass through unchanged. The agent
		// already bounds disk-usage walks to 2s; user/admin counts are tiny
		// non-negative ints; host_flags is a fixed boolean enum.
		Extras:          m.Extras,
		AgentSelfUpdate: sanitizeAgentSelfUpdate(m.AgentSelfUpdate),
	}
}

// ApplyMetadata sanitizes and stores agent-pushed metadata for a site, updating
// liveness + health. Runs in the resolved site's tenant scope. Metadata is
// best-effort telemetry: it is sanitized (truncated/capped), never rejected, so
// a sync always succeeds for any real-world plugin/theme set.
//
// The persisted JSONB shape carries `plugins`, `themes`, and (when set) the
// optional `core_update` advisory. Each Component's `available_update` is
// round-tripped via json struct tags — no migration is needed for the new
// fields (the column is JSONB).
func (s *Service) ApplyMetadata(ctx context.Context, tenantID, siteID uuid.UUID, m Metadata) (Site, error) {
	m = sanitizeMetadata(m)
	payload := map[string]any{
		"plugins": orEmptyComponents(m.Plugins),
		"themes":  orEmptyComponents(m.Themes),
	}
	if m.CoreUpdate != nil {
		payload["core_update"] = m.CoreUpdate
	}
	// ADR-037 Sprint 1, 1C — splat the sparse-metadata expansion into the
	// JSONB inventory as sibling keys to plugins/themes. The UI reads via the
	// site detail's components blob (Site.Components is the raw JSONB).
	if m.Extras != nil {
		if m.Extras.HostFlags != nil {
			payload["host_flags"] = m.Extras.HostFlags
		}
		if m.Extras.Disk != nil {
			payload["disk"] = m.Extras.Disk
		}
		if m.Extras.UserCount > 0 {
			payload["user_count"] = m.Extras.UserCount
		}
		if m.Extras.AdminCount > 0 {
			payload["admin_count"] = m.Extras.AdminCount
		}
	}
	// The agent's account of its last self-update apply beat, stored as a
	// sibling key to plugins/themes. Absent when the agent sent nothing, exactly
	// like core_update: the whole inventory blob is rewritten by every push, so
	// a site that has never staged a self-update simply never carries the key.
	if m.AgentSelfUpdate != nil {
		payload["agent_self_update"] = m.AgentSelfUpdate
	}
	components, err := json.Marshal(payload)
	if err != nil {
		return Site{}, domain.Internal("components_marshal_failed", "failed to encode site components").WithCause(err)
	}
	return s.repo.UpdateMetadata(ctx, tenantID, siteID, m, components)
}

// Heartbeat updates only liveness/health for a site.
func (s *Service) Heartbeat(ctx context.Context, tenantID, siteID uuid.UUID) error {
	return s.repo.TouchSeen(ctx, tenantID, siteID)
}

func orEmptyComponents(c []Component) []Component {
	if c == nil {
		return []Component{}
	}
	return c
}

func normalizeTags(tags []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func normalizePage(limit, offset int32) (int32, int32) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
