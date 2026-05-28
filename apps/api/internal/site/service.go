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
}

// NewService builds a site Service.
func NewService(repo Repo, v *domain.Validator, clock domain.Clock) *Service {
	return &Service{repo: repo, validator: v, clock: clock}
}

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
	return s.repo.Enroll(ctx, hashPairingCode(req.PairingCode), EnrollInput{
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
		out = append(out, Component{Slug: c.Slug, Name: c.Name, Version: c.Version, Active: c.Active})
	}
	return out
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
		out = append(out, Component{
			Slug:    truncateRunes(slug, maxComponentLen),
			Name:    truncateRunes(c.Name, maxComponentLen),
			Version: truncateRunes(c.Version, maxVersionLen),
			Active:  c.Active,
		})
		if len(out) >= maxLen {
			break
		}
	}
	return out
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
	}
}

// ApplyMetadata sanitizes and stores agent-pushed metadata for a site, updating
// liveness + health. Runs in the resolved site's tenant scope. Metadata is
// best-effort telemetry: it is sanitized (truncated/capped), never rejected, so
// a sync always succeeds for any real-world plugin/theme set.
func (s *Service) ApplyMetadata(ctx context.Context, tenantID, siteID uuid.UUID, m Metadata) (Site, error) {
	m = sanitizeMetadata(m)
	components, err := json.Marshal(map[string]any{
		"plugins": orEmptyComponents(m.Plugins),
		"themes":  orEmptyComponents(m.Themes),
	})
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
