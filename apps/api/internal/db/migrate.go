package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

// migrationsFS holds the embedded Atlas-authored versioned migrations so the
// server can apply them on startup without shipping the Atlas binary. The
// files are the single source of truth; `atlas migrate apply` produces
// identical results in environments that prefer the CLI (see README).
var migrationsFS = migrations.FS

// Migrate applies any not-yet-applied versioned migrations in lexical order.
// Applied versions are tracked in the schema_migrations table. Each migration
// runs in its own transaction; a failure rolls back that migration and aborts.
func (p *Pool) Migrate(ctx context.Context) error {
	if err := p.ensureMigrationsTable(ctx); err != nil {
		return err
	}

	applied, err := p.appliedVersions(ctx)
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationsFS, ".")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	var versions []string
	files := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		version := strings.TrimSuffix(name, ".sql")
		versions = append(versions, version)
		files[version] = name
	}
	sort.Strings(versions)

	for _, version := range versions {
		if applied[version] {
			continue
		}
		body, err := fs.ReadFile(migrationsFS, files[version])
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}
		if err := p.applyOne(ctx, version, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
	}
	return nil
}

func (p *Pool) ensureMigrationsTable(ctx context.Context) error {
	const q = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text        PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`
	if _, err := p.Exec(ctx, q); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	return nil
}

func (p *Pool) appliedVersions(ctx context.Context) (map[string]bool, error) {
	rows, err := p.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("query applied versions: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func (p *Pool) applyOne(ctx context.Context, version, body string) error {
	return pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, body); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
		return err
	})
}
