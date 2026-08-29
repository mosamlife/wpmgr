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
	// RegisterClient returns ROWS WRITTEN, mirroring the :execrows query
	// exactly. It deliberately does not return the row: the register GUC
	// enables no SELECT policy, so RETURNING raises 42501 (see Repo's method).
	// Modelling this as a count in the interface is what lets a fake reproduce
	// a zero-row write, which a row-returning shape cannot express.
	RegisterClient(ctx context.Context, arg sqlc.RegisterMCPOAuthClientParams) (int64, error)
	LookupClient(ctx context.Context, clientID string) (sqlc.McpOauthClient, error)

	LookupAuthorizationCode(ctx context.Context, codeHash string) (sqlc.GetMCPAuthorizationCodeByHashForLookupRow, error)

	// RedeemAuthorizationCode consumes the code AND issues the token in ONE
	// transaction. There is deliberately no way to do only half of it: a
	// separate consume would let a failure between the two commits burn a code
	// that never produced a token, stranding a client that did nothing wrong.
	RedeemAuthorizationCode(ctx context.Context, tenantID, codeID uuid.UUID, tok sqlc.CreateMCPConnectionTokenParams) (sqlc.McpConnectionToken, error)

	CreateGrantWithCode(ctx context.Context, g sqlc.CreateMCPGrantParams, mkCode func(grantID uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams) (sqlc.McpGrant, sqlc.McpAuthorizationCode, error)

	LookupConnectionToken(ctx context.Context, tokenHash string) (sqlc.GetMCPConnectionTokenByHashForLookupRow, error)
	ReCheckAuthorization(ctx context.Context, tenantID, tokenID uuid.UUID) (sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow, error)
	ResolveScopeSites(ctx context.Context, tenantID uuid.UUID, mode string, tagIDs, siteIDs []uuid.UUID) ([]uuid.UUID, error)

	// RecordClientIdentity stamps the connecting client's self-reported
	// identity on the grant. protocolVersion is a *string and NOT a string so
	// that "sent no header" survives as NULL -- see the method on Repo.
	RecordClientIdentity(ctx context.Context, tenantID, grantID uuid.UUID, name, version string, protocolVersion *string) error

	// ListSitesForRead reads one bounded page of the tenant's sites for the
	// list_sites tool. It returns the rows AND whether the bound was reached
	// with rows still unread, because a page-bounded list that reports itself
	// as complete is a lie about the fleet.
	ListSitesForRead(ctx context.Context, tenantID uuid.UUID, limit int32) ([]sqlc.ListSitesRow, bool, error)
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
// app.mcp_client_register and returns the number of rows written.
//
// IT RETURNS A COUNT, NOT A ROW, AND THAT IS NOT A STYLE CHOICE. The only
// policy this GUC enables is FOR INSERT, so the transaction genuinely cannot
// read the table back -- and `RETURNING` is a read. Under FORCE ROW LEVEL
// SECURITY the SELECT policy is enforced against the returned row as a
// WithCheckOption, so any RETURNING here raises SQLSTATE 42501 at
// ExecWithCheckOptions and the whole transaction rolls back. Every registration
// would fail at runtime.
//
// THIS IS NOT `RETURNING *`-SPECIFIC. Returning one column fails identically --
// even `RETURNING id`, which is server-generated and contains nothing the
// caller supplied. Reading the fix as "return fewer columns" reproduces the
// bug exactly.
//
// The obvious alternative -- setting app.mcp_client_lookup here too -- was
// rejected on purpose. Registration is an UNAUTHENTICATED POST, so granting it
// the lookup GUC would let any caller enumerate every registered client on the
// installation, client_secret_hash included. That swaps "the database refuses"
// for "the handler happens not to ask", which is the weaker guarantee.
//
// So the caller asserts the count and then reads the row back in a separate
// InMCPClientLookupTx. One extra round trip is the cost of the split.
func (r *Repo) RegisterClient(ctx context.Context, arg sqlc.RegisterMCPOAuthClientParams) (int64, error) {
	var affected int64
	err := r.pool.InMCPClientRegisterTx(ctx, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).RegisterMCPOAuthClient(ctx, arg)
		if err != nil {
			return fmt.Errorf("register mcp oauth client: %w", err)
		}
		affected = n
		return nil
	})
	return affected, err
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

// RedeemAuthorizationCode consumes the code and issues the connection token in
// ONE InTenantTx. Either both land or neither does.
//
// THE CONSUME IS STILL THE COMPARE-AND-SET, and it still runs in a transaction
// separate from the LOOKUP -- that separation is m124 obligation 1 and it is
// not what changed here. The query is :one over a predicate carrying
// `consumed_at IS NULL AND expires_at > now()`, so pgx.ErrNoRows means the code
// was NOT consumed: redeemed by a racing exchange, expired between the lookup
// and now, or refused by RLS. All three mean refuse, and the caller must never
// read "no row" as "already fine".
//
// WHY THE TOKEN INSERT JOINED IT. These were two commits. Burning the code
// before minting the token is the correct ORDER -- the reverse risks two tokens
// from one code -- but as separate commits a failure in between left a state
// that is safe and useless: the code permanently consumed, no token issued, and
// a client that did nothing wrong unable to retry and forced to restart the
// whole browser flow with nothing explaining why. One transaction removes the
// window rather than documenting it.
//
// Ordering inside the transaction is unchanged and still matters: the
// compare-and-set runs FIRST, so two concurrent exchanges still produce exactly
// one winner. The loser's INSERT never runs because its UPDATE matched nothing.
func (r *Repo) RedeemAuthorizationCode(
	ctx context.Context,
	tenantID, codeID uuid.UUID,
	tok sqlc.CreateMCPConnectionTokenParams,
) (sqlc.McpConnectionToken, error) {
	var out sqlc.McpConnectionToken
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.ConsumeMCPAuthorizationCodeInTenantTx(ctx,
			sqlc.ConsumeMCPAuthorizationCodeInTenantTxParams{TenantID: tenantID, ID: codeID}); err != nil {
			return err // pgx.ErrNoRows is meaningful to the caller
		}
		row, err := q.CreateMCPConnectionToken(ctx, tok)
		if err != nil {
			// Rolls the consume back with it. The code stays redeemable, which
			// is the whole point of the single transaction.
			return fmt.Errorf("create mcp connection token: %w", err)
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
			// COLUMN3 IS THE SITE ARRAY AND COLUMN4 IS THE TAG ARRAY, in that
			// order. The query reads `WHEN 'list' THEN s.id = ANY($3)` and
			// `WHEN 'tags' THEN ... t.id = ANY($4)`, so the positional names
			// sqlc generates do NOT follow this function's argument order.
			//
			// They were swapped here until an integration proof executed a
			// 'list' grant: both scoped modes resolved to ZERO SITES, because
			// 'list' compared site ids against tag ids and 'tags' looked up tag
			// ids by site id. It never leaked -- both directions fail closed --
			// but every grant that was not mode 'all' silently read nothing,
			// and the caller reports that as "your scope resolves to no sites"
			// for a perfectly valid grant.
			//
			// No unit test could catch it: the fake store ignores both arrays.
			sqlc.ResolveMCPGrantScopeSitesInTenantTxParams{
				TenantID: tenantID,
				Column2:  mode,
				Column3:  siteIDs,
				Column4:  tagIDs,
			})
		if err != nil {
			return fmt.Errorf("resolve mcp grant scope sites: %w", err)
		}
		out = rows
		return nil
	})
	return out, err
}

// RecordClientIdentity stamps client_name, client_version, protocol_version
// and client_identity_recorded_at on the grant.
//
// IT RUNS InTenantTx AND NOT IN THE TOKEN-LOOKUP TRANSACTION. This is m124
// obligation 1, the one that fails SILENTLY: mcp_grants' lookup policies are
// FOR SELECT, so an UPDATE issued inside the lookup transaction matches ZERO
// ROWS AND RAISES NO ERROR under FORCE ROW LEVEL SECURITY. The tenant is known
// the moment the credential resolves, so the write belongs here.
//
// protocolVersion is a *string, and nil means the client sent no
// MCP-Protocol-Version header. It MUST NOT be defaulted to a string: NULL is
// what separates "connected and sent no header" (recorded_at set,
// protocol_version NULL) from "has never connected" (recorded_at NULL), and
// that first fact is a compatibility signal an operator needs.
func (r *Repo) RecordClientIdentity(
	ctx context.Context,
	tenantID, grantID uuid.UUID,
	name, version string,
	protocolVersion *string,
) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).RecordMCPGrantClientIdentityInTenantTx(ctx,
			sqlc.RecordMCPGrantClientIdentityInTenantTxParams{
				TenantID:        tenantID,
				ID:              grantID,
				ClientName:      nullableText(name),
				ClientVersion:   nullableText(version),
				ProtocolVersion: protocolVersion,
			})
		if err != nil {
			return fmt.Errorf("record mcp client identity: %w", err)
		}
		return nil
	})
}

// ListSitesForRead reads one bounded page of the tenant's sites inside
// InTenantTx, so `sites` RLS scopes the read to this tenant regardless of what
// the grant claims.
//
// It asks for limit+1 rows and reports the overflow as `more` rather than
// silently returning a short list. A page bound that is invisible to the
// caller is how "these are your sites" becomes false for any org with more
// sites than one page.
func (r *Repo) ListSitesForRead(ctx context.Context, tenantID uuid.UUID, limit int32) ([]sqlc.ListSitesRow, bool, error) {
	var rows []sqlc.ListSitesRow
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		out, err := sqlc.New(tx).ListSites(ctx, sqlc.ListSitesParams{
			TenantID: tenantID,
			Limit:    limit + 1,
			Offset:   0,
			// Sort is bound as a parameter and compared against fixed literals
			// in the query; "name" is one of them and gives the model a stable
			// order across calls.
			Sort: "name",
		})
		if err != nil {
			return fmt.Errorf("list sites for mcp read: %w", err)
		}
		rows = out
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if int32(len(rows)) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
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
