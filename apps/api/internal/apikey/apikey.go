// Package apikey implements tenant-scoped API keys: generation of the
// `wpmgr_<prefix>_<secret>` token, sha256 hashing (only the hash + prefix are
// stored), and the create/list/revoke/authenticate operations. The full key is
// returned only once, at creation.
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// keyPrefix is the literal token prefix identifying a WPMgr API key.
const keyPrefix = "wpmgr"

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// KindIntegration is an ordinary operator-minted key. Default; may use either
// authorization model.
const KindIntegration = "integration"

// KindAgent is a machine principal minted for an automated caller. m120's
// api_keys_agent_capability_check refuses an agent key on the role model, so
// these are always least-privilege.
const KindAgent = "agent"

// APIKey is a stored key record (never includes the secret).
type APIKey struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	Name       string
	Prefix     string
	Role       authz.Role
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time

	// Kind is KindIntegration or KindAgent.
	Kind string
	// AuthModel is domain.AuthModelRole or domain.AuthModelCapability. It is
	// the sole discriminator for how this key's permissions are computed, and
	// it is NOT derivable from Capabilities: a NULL capabilities column and an
	// empty one both scan into a zero-length slice.
	AuthModel string
	// Capabilities is the explicit permission set, non-nil exactly when
	// AuthModel is domain.AuthModelCapability. Nil for a role key.
	Capabilities []string
	// SiteScope is domain.ScopeOrg or domain.ScopeSite.
	SiteScope string
	// AllowedSiteIDs is the site allowlist, non-empty only when SiteScope is
	// domain.ScopeSite. STORED, NOT ENFORCED BY RLS: no policy reads the
	// column (see the m120 header). The site boundary for these principals is
	// enforced in Go, and the enforcement chokepoint is NOT yet wired — see
	// PrincipalFor.
	AllowedSiteIDs []uuid.UUID
}

// CapabilitySpec describes a least-privilege key to be minted.
type CapabilitySpec struct {
	// Kind is KindIntegration or KindAgent. Empty defaults to KindIntegration.
	Kind string
	// Capabilities is the explicit permission set. It MUST be non-nil; an
	// empty non-nil set is legal and means zero authority. Nil is refused,
	// because nil is how a role key is represented and accepting it here would
	// mint a role key wearing a capability label.
	Capabilities []string
	// AllowedSiteIDs restricts the key to these sites. Empty means org scope.
	AllowedSiteIDs []uuid.UUID
}

// Created bundles a freshly created key with its one-time plaintext token.
type Created struct {
	Key   APIKey
	Token string // wpmgr_<prefix>_<secret> — shown once, never stored
}

// randomToken returns a lowercase base32 string of n random bytes.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.ToLower(b32.EncodeToString(buf)), nil
}

// hashSecret returns the hex sha256 of the secret portion.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// parseToken splits a presented token into its prefix and secret parts.
func parseToken(token string) (prefix, secret string, ok bool) {
	parts := strings.Split(token, "_")
	if len(parts) != 3 || parts[0] != keyPrefix {
		return "", "", false
	}
	if parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// Service manages API keys.
type Service struct {
	pool *db.Pool
}

// NewService builds an API-key Service.
func NewService(pool *db.Pool) *Service {
	return &Service{pool: pool}
}

func toModel(k sqlc.ApiKey) APIKey {
	m := APIKey{
		ID:             k.ID,
		TenantID:       k.TenantID,
		Name:           k.Name,
		Prefix:         k.Prefix,
		Role:           authz.Role(k.Role),
		CreatedAt:      k.CreatedAt,
		Kind:           k.Kind,
		AuthModel:      k.AuthModel,
		Capabilities:   k.Capabilities,
		SiteScope:      k.SiteScope,
		AllowedSiteIDs: k.AllowedSiteIds,
	}
	if k.LastUsedAt.Valid {
		t := k.LastUsedAt.Time
		m.LastUsedAt = &t
	}
	if k.RevokedAt.Valid {
		t := k.RevokedAt.Time
		m.RevokedAt = &t
	}
	return m
}

// Create generates a new key for the tenant and returns the one-time token.
func (s *Service) Create(ctx context.Context, tenantID uuid.UUID, name string, role authz.Role) (Created, error) {
	if name == "" {
		return Created{}, domain.Validation("apikey_name_required", "API key name is required")
	}
	if !role.Valid() {
		return Created{}, domain.Validation("apikey_role_invalid", "invalid API key role")
	}
	// RoleClient is portal-only; API keys must never carry it.
	if role == authz.RoleClient {
		return Created{}, domain.Validation("apikey_role_invalid", "client role cannot be assigned to an API key")
	}
	prefix, secret, err := newTokenParts()
	if err != nil {
		return Created{}, err
	}
	// m120 (#510): the three discriminator columns and the two array columns are
	// passed EXPLICITLY. Leaving them off a keyed literal is not a compile
	// error — it silently sends "" for the strings and SQL NULL for the arrays,
	// which the CHECK and NOT NULL constraints then refuse at runtime. Keep
	// every column named here even where the value equals the column default.
	return s.insert(ctx, tenantID, name, prefix, secret, insertSpec{
		role:      role,
		kind:      KindIntegration,
		authModel: domain.AuthModelRole,
		// nil, not []string{}: SQL NULL is what the role model requires, and
		// api_keys_auth_model_capabilities_check enforces the pairing.
		capabilities: nil,
		siteScope:    domain.ScopeOrg,
		// Non-nil empty, not nil: the column is NOT NULL DEFAULT '{}', and a
		// nil slice encodes as SQL NULL, which fails 23502.
		allowedSiteIDs: []uuid.UUID{},
	})
}

// CreateCapability mints a least-privilege key whose authority is an explicit
// capability set. The stored role is deliberately RoleClient — the one role
// that holds zero permissions in the matrix — so that if any future code path
// ever reaches this key's role instead of its capabilities, the answer is "no"
// rather than a silent grant. The capability model does not consult it, and
// nothing should, but a fail-safe value costs nothing here.
func (s *Service) CreateCapability(ctx context.Context, tenantID uuid.UUID, name string, spec CapabilitySpec) (Created, error) {
	if name == "" {
		return Created{}, domain.Validation("apikey_name_required", "API key name is required")
	}
	kind := spec.Kind
	if kind == "" {
		kind = KindIntegration
	}
	if kind != KindIntegration && kind != KindAgent {
		return Created{}, domain.Validation("apikey_kind_invalid", "invalid API key kind")
	}
	// Vocabulary and per-element format live here because the database
	// deliberately does not enumerate the permission strings; see
	// authz.ValidateCapabilities. An unknown capability is refused, not dropped.
	if err := authz.ValidateCapabilities(spec.Capabilities); err != nil {
		return Created{}, err
	}

	// Normalise the capability set to non-nil so it reaches Postgres as '{}'
	// rather than NULL. ValidateCapabilities already refused a nil set, so this
	// only ever converts an empty-but-non-nil slice that pgx would still encode
	// correctly — it is belt-and-braces against a future caller path.
	caps := spec.Capabilities
	if caps == nil {
		caps = []string{}
	}

	siteScope := domain.ScopeOrg
	allowed := []uuid.UUID{}
	if len(spec.AllowedSiteIDs) > 0 {
		siteScope = domain.ScopeSite
		allowed = append(allowed, spec.AllowedSiteIDs...)
	}

	prefix, secret, err := newTokenParts()
	if err != nil {
		return Created{}, err
	}
	return s.insert(ctx, tenantID, name, prefix, secret, insertSpec{
		role:           authz.RoleClient,
		kind:           kind,
		authModel:      domain.AuthModelCapability,
		capabilities:   caps,
		siteScope:      siteScope,
		allowedSiteIDs: allowed,
	})
}

// insertSpec carries the full api_keys column set for one INSERT. It exists so
// that both mint paths go through a single struct literal that names every
// m120 column, making an omission a compile error at the one place it is built
// rather than a silent zero value at each call site.
type insertSpec struct {
	role           authz.Role
	kind           string
	authModel      string
	capabilities   []string
	siteScope      string
	allowedSiteIDs []uuid.UUID
}

func newTokenParts() (prefix, secret string, err error) {
	prefix, err = randomToken(6)
	if err != nil {
		return "", "", domain.Internal("apikey_gen_failed", "failed to generate key").WithCause(err)
	}
	secret, err = randomToken(24)
	if err != nil {
		return "", "", domain.Internal("apikey_gen_failed", "failed to generate key").WithCause(err)
	}
	return prefix, secret, nil
}

func (s *Service) insert(ctx context.Context, tenantID uuid.UUID, name, prefix, secret string, spec insertSpec) (Created, error) {
	token := keyPrefix + "_" + prefix + "_" + secret
	var created Created
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
			TenantID:       tenantID,
			Name:           name,
			Prefix:         prefix,
			KeyHash:        hashSecret(secret),
			Role:           string(spec.role),
			Kind:           spec.kind,
			AuthModel:      spec.authModel,
			Capabilities:   spec.capabilities,
			SiteScope:      spec.siteScope,
			AllowedSiteIds: spec.allowedSiteIDs,
		})
		if err != nil {
			return domain.Internal("apikey_create_failed", "failed to create API key").WithCause(err)
		}
		created = Created{Key: toModel(row), Token: token}
		return nil
	})
	return created, err
}

// PrincipalFor builds the request principal for an authenticated key. The
// stored site_scope values are exactly domain.ScopeOrg / domain.ScopeSite and
// the auth_model values exactly domain.AuthModelRole /
// domain.AuthModelCapability, so this is an assignment, not a translation.
//
// NOTE (out of scope for the #510 Go half, deliberately): populating
// Scope/AllowedSiteIDs here makes authz.RequireSiteAccess enforce the allowlist
// on every by-id route that already calls it, because that middleware keys on
// Scope == domain.ScopeSite. Routes that do NOT call it — and the bulk routes
// that fan out over many sites in the handler — are not covered by this change.
func PrincipalFor(k APIKey) domain.Principal {
	p := domain.Principal{
		Type:         domain.PrincipalAPIKey,
		APIKeyID:     k.ID,
		TenantID:     k.TenantID,
		Role:         string(k.Role),
		Scope:        k.SiteScope,
		AuthModel:    k.AuthModel,
		Capabilities: k.Capabilities,
	}
	if p.Scope == "" {
		p.Scope = domain.ScopeOrg
	}
	if p.AuthModel == "" {
		p.AuthModel = domain.AuthModelRole
	}
	if p.Scope == domain.ScopeSite {
		p.AllowedSiteIDs = append([]uuid.UUID(nil), k.AllowedSiteIDs...)
	}
	return p
}

// List returns a tenant's API keys.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]APIKey, error) {
	var out []APIKey
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListAPIKeys(ctx, sqlc.ListAPIKeysParams{TenantID: tenantID, Limit: limit, Offset: offset})
		if err != nil {
			return domain.Internal("apikey_list_failed", "failed to list API keys").WithCause(err)
		}
		out = make([]APIKey, 0, len(rows))
		for _, row := range rows {
			out = append(out, toModel(row))
		}
		return nil
	})
	return out, err
}

// Revoke marks a key revoked.
func (s *Service) Revoke(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).RevokeAPIKey(ctx, sqlc.RevokeAPIKeyParams{ID: id, TenantID: tenantID})
		if err != nil {
			return domain.Internal("apikey_revoke_failed", "failed to revoke API key").WithCause(err)
		}
		if n == 0 {
			return domain.NotFound("apikey_not_found", "API key not found or already revoked")
		}
		return nil
	})
}

// Authenticate resolves a presented bearer token to its tenant + role, or
// returns an unauthorized error. It rejects revoked keys and updates
// last_used_at. The by-prefix lookup uses the dedicated lookup policy; once the
// tenant is known, the touch runs in that tenant's normal RLS scope.
func (s *Service) Authenticate(ctx context.Context, token string) (APIKey, error) {
	prefix, secret, ok := parseToken(token)
	if !ok {
		return APIKey{}, domain.Unauthorized("apikey_malformed", "malformed API key")
	}

	var row sqlc.ApiKey
	err := s.pool.InAPIKeyLookupTx(ctx, func(tx pgx.Tx) error {
		r, err := sqlc.New(tx).GetAPIKeyByPrefix(ctx, prefix)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.Unauthorized("apikey_invalid", "invalid API key")
			}
			return domain.Internal("apikey_lookup_failed", "failed to resolve API key").WithCause(err)
		}
		row = r
		return nil
	})
	if err != nil {
		return APIKey{}, err
	}

	if row.RevokedAt.Valid {
		return APIKey{}, domain.Unauthorized("apikey_revoked", "API key has been revoked")
	}
	if subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(row.KeyHash)) != 1 {
		return APIKey{}, domain.Unauthorized("apikey_invalid", "invalid API key")
	}

	// Best-effort last-used update in the key's own tenant scope.
	_ = s.pool.InTenantTx(ctx, row.TenantID, func(tx pgx.Tx) error {
		return sqlc.New(tx).TouchAPIKey(ctx, sqlc.TouchAPIKeyParams{ID: row.ID, TenantID: row.TenantID})
	})

	return toModel(row), nil
}
