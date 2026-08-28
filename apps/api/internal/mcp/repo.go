package mcp

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// Store is the database surface the service depends on. It exists as an
// interface so the single-use and revocation proofs can drive the exact Go
// branch structure with a fake that models the SQL contract faithfully --
// notably that a losing compare-and-set returns pgx.ErrNoRows rather than a
// zero-value row.
type Store interface {
	RegisterClient(ctx context.Context, arg sqlc.RegisterMCPOAuthClientParams) (sqlc.McpOauthClient, error)
	LookupClient(ctx context.Context, clientID string) (sqlc.McpOauthClient, error)

	LookupAuthorizationCode(ctx context.Context, codeHash string) (sqlc.GetMCPAuthorizationCodeByHashForLookupRow, error)
	ConsumeAuthorizationCode(ctx context.Context, tenantID, codeID uuid.UUID) (sqlc.McpAuthorizationCode, error)

	CreateGrantWithCode(ctx context.Context, g sqlc.CreateMCPGrantParams, mkCode func(grantID uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams) (sqlc.McpGrant, sqlc.McpAuthorizationCode, error)
	CreateConnectionToken(ctx context.Context, arg sqlc.CreateMCPConnectionTokenParams) (sqlc.McpConnectionToken, error)

	LookupConnectionToken(ctx context.Context, tokenHash string) (sqlc.GetMCPConnectionTokenByHashForLookupRow, error)
	ReCheckAuthorization(ctx context.Context, tenantID, tokenID uuid.UUID) (sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow, error)
	ResolveScopeSites(ctx context.Context, tenantID uuid.UUID, mode string, tagIDs, siteIDs []uuid.UUID) ([]uuid.UUID, error)
}

// Repo is the live Store. Every method names the tx helper it runs under, and
// the pairing is the security boundary rather than a style choice -- see the
// block comment above the four helpers in internal/db/db.go.
type Repo struct {
	pool *db.Pool
}

func NewRepo(pool *db.Pool) *Repo { return &Repo{pool: pool} }

var _ Store = (*Repo)(nil)

// RegisterClient inserts one RFC 7591 registration under
// app.mcp_client_register. The policy is FOR INSERT, so this transaction
// cannot read the table back.
func (r *Repo) RegisterClient(ctx context.Context, arg sqlc.RegisterMCPOAuthClientParams) (sqlc.McpOauthClient, error) {
	var out sqlc.McpOauthClient
	err := r.pool.InMCPClientRegisterTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).RegisterMCPOAuthClient(ctx, arg)
		if err != nil {
			return fmt.Errorf("register mcp oauth client: %w", err)
		}
		out = row
		return nil
	})
	return out, err
}

// LookupClient resolves a registration by client_id under
// app.mcp_client_lookup. READ ONLY: the policy is FOR SELECT.
//
// The caller must exact-match the presented redirect_uri against
// out.RedirectUris itself; there is deliberately no redirect_uri parameter to
// push that into SQL.
func (r *Repo) LookupClient(ctx context.Context, clientID string) (sqlc.McpOauthClient, error) {
	var out sqlc.McpOauthClient
	err := r.pool.InMCPClientLookupTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetMCPOAuthClientByClientIDForLookup(ctx, clientID)
		if err != nil {
			return err // pgx.ErrNoRows is meaningful to the caller
		}
		out = row
		return nil
	})
	return out, err
}

// LookupAuthorizationCode resolves a PKCE code by hash under
// app.mcp_code_lookup. READ ONLY.
//
// It deliberately does NOT consume. Consuming here is m124 obligation 1's
// silent failure: the policy is FOR SELECT, the UPDATE would match zero rows,
// raise no error, and leave the code replayable.
func (r *Repo) LookupAuthorizationCode(ctx context.Context, codeHash string) (sqlc.GetMCPAuthorizationCodeByHashForLookupRow, error) {
	var out sqlc.GetMCPAuthorizationCodeByHashForLookupRow
	err := r.pool.InMCPCodeLookupTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetMCPAuthorizationCodeByHashForLookup(ctx, codeHash)
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	return out, err
}

// ConsumeAuthorizationCode performs the atomic compare-and-set, in its own
// InTenantTx, as m124 obligation 1 requires.
//
// The query is :one over a predicate carrying `consumed_at IS NULL AND
// expires_at > now()`, so pgx.ErrNoRows means the code was NOT consumed --
// redeemed by a racing exchange, expired between the two transactions, or
// refused by RLS. All three mean refuse, and the caller must never read "no
// row" as "already fine".
func (r *Repo) ConsumeAuthorizationCode(ctx context.Context, tenantID, codeID uuid.UUID) (sqlc.McpAuthorizationCode, error) {
	var out sqlc.McpAuthorizationCode
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).ConsumeMCPAuthorizationCodeInTenantTx(ctx,
			sqlc.ConsumeMCPAuthorizationCodeInTenantTxParams{TenantID: tenantID, ID: codeID})
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	return out, err
}

// CreateGrantWithCode mints the grant and its first authorization code in ONE
// InTenantTx, so a crash between them cannot leave a grant nobody can redeem
// or a code pointing at no grant. mkCode receives the new grant id.
//
// Not run under app.site_scope: mcp_grants_site_scope_insert is RESTRICTIVE FOR
// INSERT, so a site-scoped collaborator inserting here would match nothing.
// Minting a grant is an operator action and the handler gates it accordingly.
func (r *Repo) CreateGrantWithCode(
	ctx context.Context,
	g sqlc.CreateMCPGrantParams,
	mkCode func(grantID uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams,
) (sqlc.McpGrant, sqlc.McpAuthorizationCode, error) {
	var (
		grant sqlc.McpGrant
		code  sqlc.McpAuthorizationCode
	)
	err := r.pool.InTenantTx(ctx, g.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		gr, err := q.CreateMCPGrant(ctx, g)
		if err != nil {
			return fmt.Errorf("create mcp grant: %w", err)
		}
		cd, err := q.CreateMCPAuthorizationCode(ctx, mkCode(gr.ID))
		if err != nil {
			return fmt.Errorf("create mcp authorization code: %w", err)
		}
		grant, code = gr, cd
		return nil
	})
	return grant, code, err
}

// CreateConnectionToken stores the hashed bearer token under InTenantTx.
func (r *Repo) CreateConnectionToken(ctx context.Context, arg sqlc.CreateMCPConnectionTokenParams) (sqlc.McpConnectionToken, error) {
	var out sqlc.McpConnectionToken
	err := r.pool.InTenantTx(ctx, arg.TenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).CreateMCPConnectionToken(ctx, arg)
		if err != nil {
			return fmt.Errorf("create mcp connection token: %w", err)
		}
		out = row
		return nil
	})
	return out, err
}

// LookupConnectionToken resolves a bearer token by hash under
// app.mcp_token_lookup. READ ONLY, and it resolves the TOKEN ONLY.
//
// Do not be tempted into `token JOIN grant` here: mcp_grants has no lookup
// policy, only tenant isolation, so the join matches zero rows inside this
// transaction and would fail every MCP request with nothing in any log.
// Authentication is two queries -- this one to learn the tenant, then
// ReCheckAuthorization under that tenant.
func (r *Repo) LookupConnectionToken(ctx context.Context, tokenHash string) (sqlc.GetMCPConnectionTokenByHashForLookupRow, error) {
	var out sqlc.GetMCPConnectionTokenByHashForLookupRow
	err := r.pool.InMCPTokenLookupTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetMCPConnectionTokenByHashForLookup(ctx, tokenHash)
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	return out, err
}

// ReCheckAuthorization re-reads grant and token state under the resolved
// tenant. It runs on EVERY request so revocation bites on the next one rather
// than at token expiry, and the caller must branch on the row's Authorized
// column, never on row presence.
func (r *Repo) ReCheckAuthorization(ctx context.Context, tenantID, tokenID uuid.UUID) (sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow, error) {
	var out sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).ReCheckMCPRequestAuthorizationInTenantTx(ctx,
			sqlc.ReCheckMCPRequestAuthorizationInTenantTxParams{TenantID: tenantID, ID: tokenID})
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	return out, err
}

// ResolveScopeSites is the ONE audited chokepoint of m124 obligation 2. It runs
// inside InTenantTx so that joining through `sites` under tenant isolation
// drops every foreign UUID -- scope_site_ids is a uuid[] and PostgreSQL has no
// foreign key over array elements, so the column accepts any UUID at all.
//
// An empty result means NO SITES. The caller enforces that; the database
// cannot. See NewSiteSet, whose zero value allows nothing.
func (r *Repo) ResolveScopeSites(ctx context.Context, tenantID uuid.UUID, mode string, tagIDs, siteIDs []uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ResolveMCPGrantScopeSitesInTenantTx(ctx,
			sqlc.ResolveMCPGrantScopeSitesInTenantTxParams{
				TenantID: tenantID,
				Column2:  mode,
				Column3:  tagIDs,
				Column4:  siteIDs,
			})
		if err != nil {
			return fmt.Errorf("resolve mcp grant scope sites: %w", err)
		}
		out = rows
		return nil
	})
	return out, err
}

// nullableText is the pgtype-free helper for the *string columns sqlc emits.
func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// uuidToPG converts a uuid to the pgtype form CreateMCPGrantParams wants,
// mapping uuid.Nil to NULL rather than to a zero uuid that looks like a real
// author.
func uuidToPG(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}
