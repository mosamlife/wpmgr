package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// InstallBootstrapLockKey namespaces the install-wide advisory lock that
// serialises first-run ownership. Same shape as MemberRolesLockKey and
// org.LifecycleLockKey — pg_advisory_xact_lock, released by COMMIT or ROLLBACK,
// so it can never leak — but keyed on the install rather than on a tenant,
// because the whole point of the critical section is that no tenant exists yet.
//
// A DIFFERENT namespace to org_member_roles and org_lifecycle on purpose: this
// lock is taken exactly once in an install's life, by a path that has no tenant
// to name, and it guards a different invariant to either of theirs.
const InstallBootstrapLockKey = "install_bootstrap"

// OwnershipEstablished reports whether ANY owner membership exists on this
// install, across every tenant.
//
// IT REPLACES A USER COUNT, AND THAT SUBSTITUTION WAS THE DEFECT. "Has this
// install been claimed?" is a question about ownership, and a user row is not
// ownership. Any path that can create a user — a social sign-in creates one
// from a provider handshake alone — would otherwise flip the install out of
// "unclaimed" without anybody owning it, and every gate downstream then reads
// the wrong answer: first-run ownership refuses the operator's correct claim
// forever, and self-serve opens on an install that has no owner. Asking for the
// property directly is the only version that cannot be flipped by creating
// something that is not an owner.
//
// RLS: memberships is FORCE ROW LEVEL SECURITY and this read spans every
// tenant, so it runs under InAgentTx and the memberships_agent policy — the
// SELECT-only cross-tenant scope the schema already provides for exactly this
// kind of backend-only question. No tenant scope could serve instead: at the
// moment the question matters there may be no tenant at all.
//
// It has no "unknown" answer of its own, so each caller decides what an error
// means for it. Both treat an unreadable answer as "claimed", which refuses
// rather than grants.
func (r *Repo) OwnershipEstablished(ctx context.Context) (bool, error) {
	var exists bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM memberships WHERE role = $1)`,
			string(authz.RoleOwner),
		).Scan(&exists)
	})
	if err != nil {
		return false, domain.Internal("ownership_probe_failed", "failed to determine install ownership").WithCause(err)
	}
	return exists, nil
}

// BootstrapInstall creates the install's first tenant, its first user and the
// owner membership binding them, as ONE transaction under
// InstallBootstrapLockKey.
//
// THE COUNT AND THE CREATE ARE INSEPARABLE. Read "are there zero users?" in one
// statement and act on it in the next, and two callers both read zero and both
// act: two organisations, two owners, and an install whose ownership is decided
// by whichever request happened to commit second. The count below is taken
// after the advisory lock, inside the transaction that performs the writes, so
// the second caller does not read at all until the first has committed — and
// then reads one, not zero.
//
// It returns domain.Forbidden("registration_closed") when the install already
// has a user. That is the same error the caller returns for every other refusal
// on this path, deliberately: an unauthenticated caller learns "no" and nothing
// else about the install's state.
//
// RLS: memberships is FORCE ROW LEVEL SECURITY with a WITH CHECK on
// app.tenant_id, so the membership INSERT is only legal once the new tenant is
// in scope. The tenant is created first and scopeTenant (owned by internal/db,
// never by this repo) brings it into scope for the remainder of the SAME
// transaction. tenants and users carry no RLS. tenant_id is still written
// explicitly on the membership rather than left to the policy.
func (r *Repo) BootstrapInstall(
	ctx context.Context,
	email, passwordHash, name, tenantName, tenantSlug string,
) (User, Membership, uuid.UUID, error) {
	var (
		outUser  User
		outMem   Membership
		outTenID uuid.UUID
	)

	err := r.pool.InInstallLockTx(ctx, InstallBootstrapLockKey, func(tx pgx.Tx, scopeTenant func(uuid.UUID) error) error {
		q := sqlc.New(tx)

		// OWNERSHIP, NOT POPULATION. Read inside the locked transaction that
		// will perform the writes, so the second caller does not read until the
		// first has committed. It asks whether an owner membership exists —
		// never whether a user row does — so nothing that merely creates an
		// account can close this door on the person holding the claim.
		//
		// app.agent is set for this one statement rather than by InAgentTx,
		// because the read has to happen in THIS transaction (the one holding
		// the lock) and a second transaction would put the check back outside
		// the critical section, which is the shape the lock exists to prevent.
		// It is scoped to the transaction, so it lapses at COMMIT or ROLLBACK
		// alongside the lock; the membership INSERT below still runs under
		// app.tenant_id and its own tenant policy.
		if _, err := tx.Exec(ctx, "SELECT set_config('app.agent', 'on', true)"); err != nil {
			return domain.Internal("ownership_probe_failed", "failed to determine install ownership").WithCause(err)
		}
		var owned bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM memberships WHERE role = $1)`,
			string(authz.RoleOwner),
		).Scan(&owned); err != nil {
			return domain.Internal("ownership_probe_failed", "failed to determine install ownership").WithCause(err)
		}
		if owned {
			return errRegistrationClosed()
		}

		tenantID, err := createTenantInTx(ctx, tx, tenantName, tenantSlug)
		if err != nil {
			return err
		}
		if err := scopeTenant(tenantID); err != nil {
			return domain.Internal("tenant_scope_failed", "failed to scope the new organisation").WithCause(err)
		}

		userRow, err := q.CreateUser(ctx, sqlc.CreateUserParams{
			Email:        email,
			PasswordHash: strPtr(passwordHash),
			OidcSubject:  nil,
			OidcIssuer:   nil,
			Name:         name,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.Conflict("user_exists", "a user with this email already exists").WithCause(err)
			}
			return domain.Internal("user_create_failed", "failed to create user").WithCause(err)
		}

		memRow, err := q.CreateMembership(ctx, sqlc.CreateMembershipParams{
			UserID:   userRow.ID,
			TenantID: tenantID,
			Role:     string(authz.RoleOwner),
		})
		if err != nil {
			return domain.Internal("membership_create_failed", "failed to create membership").WithCause(err)
		}

		outUser = userToModel(userRow)
		outMem = membershipToModel(memRow)
		outTenID = tenantID
		return nil
	})
	if err != nil {
		return User{}, Membership{}, uuid.Nil, err
	}
	return outUser, outMem, outTenID, nil
}

// createTenantInTx inserts the first tenant inside the bootstrap transaction,
// mirroring tenant.Repo.Create's slug behaviour: the slug is globally unique,
// so a collision retries with a random suffix rather than hard-failing.
//
// Each attempt runs in its own SAVEPOINT. A unique violation aborts the
// enclosing transaction in Postgres, so retrying without one would fail every
// subsequent statement with "current transaction is aborted" — including the
// user and membership inserts this bootstrap exists to perform. The advisory
// lock is held by the outer transaction and is unaffected by a savepoint
// rollback.
func createTenantInTx(ctx context.Context, tx pgx.Tx, name, slug string) (uuid.UUID, error) {
	base := slug
	if len(base) > 50 {
		base = base[:50]
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		try := slug
		if attempt > 0 {
			suf := make([]byte, 3)
			if _, err := rand.Read(suf); err != nil {
				return uuid.Nil, domain.Internal("tenant_create_failed", "failed to create organisation").WithCause(err)
			}
			try = base + "-" + hex.EncodeToString(suf)
		}

		sp, err := tx.Begin(ctx)
		if err != nil {
			return uuid.Nil, domain.Internal("tenant_create_failed", "failed to create organisation").WithCause(err)
		}
		row, err := sqlc.New(sp).CreateTenant(ctx, sqlc.CreateTenantParams{Name: name, Slug: try})
		if err == nil {
			if cerr := sp.Commit(ctx); cerr != nil {
				return uuid.Nil, domain.Internal("tenant_create_failed", "failed to create organisation").WithCause(cerr)
			}
			return row.ID, nil
		}
		_ = sp.Rollback(ctx)

		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			lastErr = err
			continue // slug taken — retry with a fresh random suffix
		}
		return uuid.Nil, domain.Internal("tenant_create_failed", "failed to create organisation").WithCause(err)
	}
	return uuid.Nil, domain.Conflict("tenant_slug_exists", "could not allocate an organisation slug").WithCause(lastErr)
}
