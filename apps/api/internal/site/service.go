package site

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/google/uuid"

	agentpkg "github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Service holds site business logic. All operations require a tenant ID, which
// the handler derives from request context (tenant middleware) — except the
// enrollment and agent paths, which resolve the tenant from a pairing code or
// the agent's verified identity respectively.
type Service struct {
	repo      Repo
	validator *domain.Validator
	clock     domain.Clock
	// conn is the M21 connection-lifecycle service. Optional: when wired, a
	// site-bound enrollment code transitions the bound site (the live-enroll
	// flow); a legacy NULL-site_id code keeps the create-at-enroll path. nil ⇒
	// every code uses the legacy path (back-compat).
	conn ConnectionService
}

// NewService builds a site Service.
func NewService(repo Repo, v *domain.Validator, clock domain.Clock) *Service {
	return &Service{repo: repo, validator: v, clock: clock}
}

// SetConnectionService wires the M21 lifecycle service into the enroll branch.
// Call once at boot (the lifecycle service depends on this Service's repo, so
// it is constructed after and injected here).
func (s *Service) SetConnectionService(cs ConnectionService) { s.conn = cs }

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

// List returns a page of the tenant's sites, optionally filtered by tag.
func (s *Service) List(ctx context.Context, in ListInput) ([]Site, error) {
	if in.TenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	in.Limit, in.Offset = normalizePage(in.Limit, in.Offset)
	return s.repo.List(ctx, in)
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
	Tags           []string
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
		WPVersion:   m.WPVersion,
		PHPVersion:  m.PHPVersion,
		ServerInfo:  m.ServerInfo,
		Multisite:   m.Multisite,
		ActiveTheme: m.ActiveTheme,
		Plugins:     fromAgentComponents(m.Plugins),
		Themes:      fromAgentComponents(m.Themes),
		CoreUpdate:  fromAgentCoreUpdate(m.CoreUpdate),
		// ADR-037 Sprint 1, 1C — sparse-metadata expansion. The new fields are
		// stored inside the existing JSONB `components` column (no migration);
		// the strict ogen Site type round-trips them as raw JSON inside the
		// components blob. UI reads via the components.host_flags /
		// components.disk / components.user_count / components.admin_count
		// path. Old agents send none of these and the fields stay absent.
		Extras: fromAgentMetadataExtras(m),
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

func fromAgentCoreUpdate(cu *agentpkg.CoreUpdate) *CoreUpdate {
	if cu == nil || cu.NewVersion == "" {
		return nil
	}
	return &CoreUpdate{NewVersion: cu.NewVersion, CurrentVersion: cu.CurrentVersion}
}

// Metadata sanitization bounds. Metadata is best-effort telemetry from
// arbitrary real-world sites, so we never reject a sync over field lengths —
// we truncate (on rune boundaries) and cap slice sizes instead.
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
		if c.AvailableUpdate != nil {
			nv := strings.TrimSpace(c.AvailableUpdate.NewVersion)
			if nv != "" {
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
// whole advisory when new_version is empty (the contract requires it).
func sanitizeCoreUpdate(cu *CoreUpdate) *CoreUpdate {
	if cu == nil {
		return nil
	}
	nv := truncateRunes(strings.TrimSpace(cu.NewVersion), maxWPVersion)
	if nv == "" {
		return nil
	}
	return &CoreUpdate{
		NewVersion:     nv,
		CurrentVersion: truncateRunes(cu.CurrentVersion, maxWPVersion),
	}
}

// sanitizeMetadata coerces arbitrary agent-reported metadata into the stored
// bounds without ever erroring: scalar fields are truncated on rune
// boundaries, components with empty slugs are dropped, and the plugin/theme
// slices are capped. This is the single source of truth for metadata bounds.
func sanitizeMetadata(m Metadata) Metadata {
	return Metadata{
		WPVersion:   truncateRunes(m.WPVersion, maxWPVersion),
		PHPVersion:  truncateRunes(m.PHPVersion, maxPHPVersion),
		ServerInfo:  truncateRunes(m.ServerInfo, maxServerInfo),
		Multisite:   m.Multisite,
		ActiveTheme: truncateRunes(m.ActiveTheme, maxActiveTheme),
		Plugins:     sanitizeComponents(m.Plugins, maxPlugins),
		Themes:      sanitizeComponents(m.Themes, maxThemes),
		CoreUpdate:  sanitizeCoreUpdate(m.CoreUpdate),
		// ADR-037 Sprint 1, 1C — Extras pass through unchanged. The agent
		// already bounds disk-usage walks to 2s; user/admin counts are tiny
		// non-negative ints; host_flags is a fixed boolean enum.
		Extras: m.Extras,
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
