package riverutil

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// pgErrInsufficientPrivilege is the Postgres SQLSTATE for a permission-denied
// error (e.g. CREATE SCHEMA without database-level CREATE).
const pgErrInsufficientPrivilege = "42501"

// safeIdentifierRE matches a simple Postgres identifier: letters, digits, and
// underscores, not starting with a digit. Used to reject quoted/dotted values
// that would change the intended object reference.
var safeIdentifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// NormalizeSchema trims and validates an optional Postgres schema identifier.
func NormalizeSchema(raw string) (string, error) {
	schema := strings.TrimSpace(raw)
	if schema == "" {
		return "", nil
	}
	if !safeIdentifierRE.MatchString(schema) {
		return "", fmt.Errorf("invalid River schema %q: use a simple Postgres identifier", raw)
	}
	return schema, nil
}

// IsDefaultSchema reports whether schema should use the connection's default
// search_path behavior.
func IsDefaultSchema(schema string) bool {
	return schema == "" || strings.EqualFold(schema, "public")
}

// QualifiedTable returns a validated table reference for raw SQL touching River
// tables. Queue names and all other values must remain SQL parameters.
func QualifiedTable(schema, table string) (string, error) {
	table = strings.TrimSpace(table)
	if !safeIdentifierRE.MatchString(table) {
		return "", fmt.Errorf("invalid River table %q: use a simple Postgres identifier", table)
	}
	schema, err := NormalizeSchema(schema)
	if err != nil {
		return "", err
	}
	if IsDefaultSchema(schema) {
		return table, nil
	}
	return pgx.Identifier{schema, table}.Sanitize(), nil
}

// EnsureSchema creates, grants, and migrates a non-default River schema. Empty
// and public schemas intentionally keep the existing single-schema behavior.
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool, schema, appRole string) error {
	schema, err := NormalizeSchema(schema)
	if err != nil {
		return err
	}
	if IsDefaultSchema(schema) {
		return nil
	}
	appRole = strings.TrimSpace(appRole)
	if appRole == "" {
		return fmt.Errorf("app role is required for River schema grants")
	}

	schemaIdent := pgx.Identifier{schema}.Sanitize()
	roleIdent := pgx.Identifier{appRole}.Sanitize()

	// GH #207 Bug 1: readiness fast-path. Once the schema exists and has been
	// River-migrated, EVERY boot after the first needs only the read/USAGE
	// privileges the app role already holds (granted by the FIRST creation's
	// GRANTs below) — it must never re-run the privileged CREATE SCHEMA /
	// GRANT / ALTER DEFAULT PRIVILEGES statements. Those statements require
	// DATABASE-level CREATE even when the schema already exists (Postgres
	// checks the privilege before the IF-NOT-EXISTS short-circuit runs), so a
	// process connecting with the unprivileged app role (e.g. the
	// media-encoder when no WPMGR_DB_MIGRATION_DSN owner DSN is configured on
	// that service) crash-looped on every boot with SQLSTATE 42501 even
	// though the schema was already fully set up.
	//
	// to_regclass resolves a schema-qualified name only when a matching
	// relation exists AND is visible to the connected role — the app role's
	// standing USAGE grant on the schema (from the original creation) is
	// exactly what makes it visible, so a true result here is proof the
	// schema is ready without ever touching a privileged DDL path.
	qualifiedRiverJob := pgx.Identifier{schema, "river_job"}.Sanitize()
	var ready bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", qualifiedRiverJob).Scan(&ready); err != nil {
		return fmt.Errorf("probe River schema %q readiness: %w", schema, err)
	}
	if ready {
		return nil
	}

	preMigration := []string{
		fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schemaIdent),
		fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", schemaIdent, roleIdent),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s", schemaIdent, roleIdent),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT USAGE, SELECT ON SEQUENCES TO %s", schemaIdent, roleIdent),
	}
	for _, stmt := range preMigration {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return wrapSchemaPermissionErr(schema, err)
		}
	}

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: schema})
	if err != nil {
		return fmt.Errorf("river migrator for schema %q: %w", schema, err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate schema %q: %w", schema, err)
	}

	postMigration := []string{
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO %s", schemaIdent, roleIdent),
		fmt.Sprintf("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA %s TO %s", schemaIdent, roleIdent),
	}
	for _, stmt := range postMigration {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return wrapSchemaPermissionErr(schema, err)
		}
	}
	return nil
}

// wrapSchemaPermissionErr detects a Postgres insufficient_privilege error
// (SQLSTATE 42501) on the privileged first-time River-schema setup path and
// replaces the raw pg error with an actionable message. Unlike the readiness
// fast-path above (which only ever needs the app role's own read/USAGE
// grants), first-time creation of a dedicated River schema needs an
// owner-level role — surface that distinction instead of a bare pg error. The
// original error remains available via errors.Unwrap/errors.Is.
func wrapSchemaPermissionErr(schema string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrInsufficientPrivilege {
		return fmt.Errorf(
			"prepare River schema %q: permission denied (SQLSTATE %s) — this schema has never been "+
				"created, which requires an owner/superuser role; configure WPMGR_DB_MIGRATION_DSN with "+
				"an owner role (or grant the app role CREATE on the database), then retry: %w",
			schema, pgErrInsufficientPrivilege, err,
		)
	}
	return fmt.Errorf("prepare River schema %q: %w", schema, err)
}
