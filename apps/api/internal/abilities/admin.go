package abilities

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// The superadmin catalogue write. The SQL function is the only write path; it
// refuses unless the actor is a superadmin, serialises writers of one name on
// an advisory lock, and writes ability_catalogue_audit in the same statement.
// ---------------------------------------------------------------------------

// CatalogueInput is one admin write. Every field is optional on update:
// omitted fields keep their stored values (merged inside the write's
// transaction, under the per-name lock the SQL function also takes).
type CatalogueInput struct {
	Name               *string          `json:"name"`
	Source             *string          `json:"source"`
	Class              *string          `json:"class"`
	Status             *string          `json:"status"`
	Enabled            *bool            `json:"enabled"`
	ApprovalMode       *string          `json:"approval_mode"`
	PermissionMode     *string          `json:"permission_mode"`
	IntegrationID      *string          `json:"integration_id"`
	OwnerDir           *string          `json:"owner_dir"`
	VersionMin         *string          `json:"version_min"`
	VersionMaxTested   *string          `json:"version_max_tested"`
	MinWPVersion       *string          `json:"min_wp_version"`
	MinAgentVersion    *string          `json:"min_agent_version"`
	SchemaStructSHA256 *string          `json:"schema_struct_sha256"`
	DynamicEnumPaths   *[]string        `json:"dynamic_enum_paths"`
	Title              *string          `json:"title"`
	Description        *string          `json:"description"`
	Usage              *string          `json:"usage"`
	OperatorPermission *string          `json:"operator_permission"`
	Target             *json.RawMessage `json:"target"`
	Snapshot           *string          `json:"snapshot"`
	Preview            *string          `json:"preview"`
	ArgRender          *json.RawMessage `json:"arg_render"`
	EffectCopy         *string          `json:"effect_copy"`
	Limits             *json.RawMessage `json:"limits"`
	NestedAllow        *[]string        `json:"nested_allow"`
	GlobalOptionKeys   *[]string        `json:"global_option_keys"`
	IntegrationBlock   *json.RawMessage `json:"integration_block"`
	Admission          *json.RawMessage `json:"admission"`
}

// nullableRaw turns a JSON null into a nil column value.
func nullableRaw(r json.RawMessage) []byte {
	if len(r) == 0 || string(r) == "null" {
		return nil
	}
	return r
}

// merge overlays the supplied fields on base.
func (in CatalogueInput) merge(base sqlc.AbilityCatalogue) sqlc.AbilityCatalogue {
	s := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	p := func(dst **string, v *string) {
		if v != nil {
			if *v == "" {
				*dst = nil
			} else {
				x := *v
				*dst = &x
			}
		}
	}
	r := func(dst *[]byte, v *json.RawMessage) {
		if v != nil {
			*dst = nullableRaw(*v)
		}
	}
	a := func(dst *[]string, v *[]string) {
		if v != nil {
			*dst = *v
		}
	}
	s(&base.Name, in.Name)
	s(&base.Source, in.Source)
	s(&base.Class, in.Class)
	s(&base.Status, in.Status)
	if in.Enabled != nil {
		base.Enabled = *in.Enabled
	}
	s(&base.ApprovalMode, in.ApprovalMode)
	s(&base.PermissionMode, in.PermissionMode)
	p(&base.IntegrationID, in.IntegrationID)
	p(&base.OwnerDir, in.OwnerDir)
	p(&base.VersionMin, in.VersionMin)
	p(&base.VersionMaxTested, in.VersionMaxTested)
	p(&base.MinWpVersion, in.MinWPVersion)
	p(&base.MinAgentVersion, in.MinAgentVersion)
	p(&base.SchemaStructSha256, in.SchemaStructSHA256)
	a(&base.DynamicEnumPaths, in.DynamicEnumPaths)
	s(&base.Title, in.Title)
	s(&base.Description, in.Description)
	p(&base.Usage, in.Usage)
	p(&base.OperatorPermission, in.OperatorPermission)
	r(&base.Target, in.Target)
	s(&base.Snapshot, in.Snapshot)
	p(&base.Preview, in.Preview)
	r(&base.ArgRender, in.ArgRender)
	s(&base.EffectCopy, in.EffectCopy)
	r(&base.Limits, in.Limits)
	a(&base.NestedAllow, in.NestedAllow)
	a(&base.GlobalOptionKeys, in.GlobalOptionKeys)
	r(&base.IntegrationBlock, in.IntegrationBlock)
	r(&base.Admission, in.Admission)
	return base
}

// newEntryDefaults is the base a create starts from.
func newEntryDefaults() sqlc.AbilityCatalogue {
	return sqlc.AbilityCatalogue{
		PermissionMode: "principal", ApprovalMode: "per_call", Snapshot: "none", EffectCopy: "none",
		ArgRender: []byte(`{}`), Limits: []byte(`{}`), Admission: []byte(`{}`),
		DynamicEnumPaths: []string{}, NestedAllow: []string{}, GlobalOptionKeys: []string{},
	}
}

// mapCatalogueErr maps the SQL function's refusals to domain errors.
func mapCatalogueErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42501":
			return domain.Forbidden("superadmin_required", "superadmin access required")
		case "23514", "23502", "22P02":
			return domain.Validation("invalid_entry", "entry failed a database check")
		case "22023":
			return domain.Validation("name_immutable", "an entry's name cannot change")
		case "P0002":
			return domain.NotFound("entry_not_found", "catalogue entry not found")
		case "23505":
			return domain.Conflict("entry_conflict", "an entry with this name and version range exists")
		}
	}
	return err
}

// canonicalIntPattern is the only number spelling a catalogue JSON member may
// use: an integer as Postgres's jsonb prints it. A float, an exponent (1e3)
// or a leading zero would be re-spelled by jsonb on store, so the entry bytes
// hashed from the request would differ from the bytes reproduced from the
// stored row. The engine is integers-only anyway (v4 §2.2).
var canonicalIntPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,17})$`)

// requireCanonicalJSONNumbers refuses a JSON member holding a number jsonb
// would re-spell.
func requireCanonicalJSONNumbers(r sqlc.AbilityCatalogue) error {
	for field, raw := range map[string][]byte{
		"target": r.Target, "arg_render": r.ArgRender, "limits": r.Limits,
		"integration_block": r.IntegrationBlock, "admission": r.Admission,
	} {
		if len(raw) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		for {
			tok, err := dec.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return domain.Validation("invalid_entry", field+" is not valid JSON")
			}
			if n, ok := tok.(json.Number); ok && !canonicalIntPattern.MatchString(n.String()) {
				return domain.Validation("invalid_entry",
					field+" may hold integers only, written plainly (for example 1000, not 1e3 or 1.0)")
			}
		}
	}
	return nil
}

// AdminRepo is the catalogue admin's database access.
type AdminRepo struct{ pool *db.Pool }

// NewAdminRepo builds it.
func NewAdminRepo(pool *db.Pool) *AdminRepo { return &AdminRepo{pool: pool} }

// List reads every entry. The table is global and readable in any
// transaction; InUserTx keeps the actor on the connection.
func (r *AdminRepo) List(ctx context.Context, actor uuid.UUID) ([]sqlc.AbilityCatalogue, error) {
	var out []sqlc.AbilityCatalogue
	err := r.pool.InUserTx(ctx, actor, func(tx pgx.Tx) error {
		var err error
		out, err = sqlc.New(tx).ListAbilityCatalogue(ctx)
		return err
	})
	return out, err
}

// Upsert writes one entry. entryID nil creates. For an update the stored row
// is read, merged and written in ONE transaction, under the same per-name
// advisory lock the SQL function takes (re-entrant within the transaction),
// so a concurrent writer cannot interleave between the read and the write.
// entry_sha256 is stamped from EntryBytes of the merged row, and the bytes
// reproduced from the row the database returned must hash the same, or the
// transaction rolls back.
func (r *AdminRepo) Upsert(ctx context.Context, actor uuid.UUID, entryID *uuid.UUID, in CatalogueInput) (sqlc.AbilityCatalogue, error) {
	var out sqlc.AbilityCatalogue
	err := r.pool.InUserTx(ctx, actor, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		base := newEntryDefaults()
		if entryID != nil {
			cur, err := q.GetAbilityCatalogueEntry(ctx, *entryID)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("entry_not_found", "catalogue entry not found")
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ability_catalogue'), hashtext($1))`, cur.Name); err != nil {
				return err
			}
			// Re-read under the lock: the pre-lock read only named the key.
			if cur, err = q.GetAbilityCatalogueEntry(ctx, *entryID); err != nil {
				return err
			}
			base = cur
		} else if in.Name == nil {
			return domain.Validation("invalid_entry", "name is required")
		}
		row := in.merge(base)
		if err := requireCanonicalJSONNumbers(row); err != nil {
			return err
		}
		_, sum, err := EntryBytes(row)
		if err != nil {
			return err
		}
		var id pgtype.UUID
		if entryID != nil {
			id = pgtype.UUID{Bytes: *entryID, Valid: true}
		}
		stored, err := q.AdminUpsertAbilityCatalogueEntry(ctx, sqlc.AdminUpsertAbilityCatalogueEntryParams{
			ActorUserID: actor, EntryID: id, Name: row.Name, Source: row.Source, Class: row.Class,
			Status: row.Status, Enabled: row.Enabled, ApprovalMode: row.ApprovalMode,
			PermissionMode: row.PermissionMode, IntegrationID: row.IntegrationID, OwnerDir: row.OwnerDir,
			VersionMin: row.VersionMin, VersionMaxTested: row.VersionMaxTested,
			MinWpVersion: row.MinWpVersion, MinAgentVersion: row.MinAgentVersion,
			SchemaStructSha256: row.SchemaStructSha256, DynamicEnumPaths: nonNil(row.DynamicEnumPaths),
			Title: row.Title, Description: row.Description, Usage: row.Usage,
			OperatorPermission: row.OperatorPermission, Target: row.Target, Snapshot: row.Snapshot,
			Preview: row.Preview, ArgRender: row.ArgRender, EffectCopy: row.EffectCopy, Limits: row.Limits,
			NestedAllow: nonNil(row.NestedAllow), GlobalOptionKeys: nonNil(row.GlobalOptionKeys),
			IntegrationBlock: row.IntegrationBlock, Admission: row.Admission, EntrySha256: &sum,
			OutputFields: row.OutputFields,
		})
		if err != nil {
			return mapCatalogueErr(err)
		}
		if _, again, err := EntryBytes(stored); err != nil || again != sum {
			return domain.Validation("entry_not_reproducible",
				"the stored entry does not reproduce the bytes its hash was computed over")
		}
		out = stored
		return nil
	})
	if err != nil {
		if de, ok := domain.AsDomain(err); ok {
			return sqlc.AbilityCatalogue{}, de
		}
		return sqlc.AbilityCatalogue{}, fmt.Errorf("upsert ability catalogue entry: %w", mapCatalogueErr(err))
	}
	return out, nil
}
