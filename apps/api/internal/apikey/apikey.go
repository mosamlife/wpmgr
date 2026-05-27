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
		ID:        k.ID,
		TenantID:  k.TenantID,
		Name:      k.Name,
		Prefix:    k.Prefix,
		Role:      authz.Role(k.Role),
		CreatedAt: k.CreatedAt,
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
	prefix, err := randomToken(6)
	if err != nil {
		return Created{}, domain.Internal("apikey_gen_failed", "failed to generate key").WithCause(err)
	}
	secret, err := randomToken(24)
	if err != nil {
		return Created{}, domain.Internal("apikey_gen_failed", "failed to generate key").WithCause(err)
	}
	token := keyPrefix + "_" + prefix + "_" + secret

	var created Created
	err = s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
			TenantID: tenantID,
			Name:     name,
			Prefix:   prefix,
			KeyHash:  hashSecret(secret),
			Role:     string(role),
		})
		if err != nil {
			return domain.Internal("apikey_create_failed", "failed to create API key").WithCause(err)
		}
		created = Created{Key: toModel(row), Token: token}
		return nil
	})
	return created, err
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
