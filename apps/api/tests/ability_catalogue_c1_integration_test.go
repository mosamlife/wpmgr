package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// m159 (engine E3, D1). Every write below goes through the SECURITY DEFINER
// admin_upsert_ability_catalogue_entry on the wpmgr_app pool inside InUserTx,
// the path the admin repo takes.

const c1SchemaHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func c1Str(s string) *string { return &s }

// c1VendorRead is an admitted vendor read with a pinned schema and output.
func c1VendorRead(actor uuid.UUID, name, vmin, vmax string) sqlc.AdminUpsertAbilityCatalogueEntryParams {
	p := sqlc.AdminUpsertAbilityCatalogueEntryParams{
		ActorUserID: actor, Name: name, Source: "vendor", Class: "read", Status: "admitted",
		Enabled: true, ApprovalMode: "none", PermissionMode: "principal",
		OwnerDir: c1Str("c1-builder"), SchemaStructSha256: c1Str(c1SchemaHash),
		DynamicEnumPaths: []string{}, Title: "A vendor read", Description: "Reads a vendor thing.",
		Snapshot: "none", ArgRender: []byte("{}"), EffectCopy: "none", Limits: []byte("{}"),
		NestedAllow: []string{}, GlobalOptionKeys: []string{}, Admission: []byte("{}"),
		OutputFields: []byte(`{"fields":{"id":"int","items":{"items":{"fields":{"label":"string"}}}}}`),
	}
	if vmin != "" {
		p.VersionMin = c1Str(vmin)
	}
	if vmax != "" {
		p.VersionMaxTested = c1Str(vmax)
	}
	return p
}

func c1Upsert(ctx context.Context, app *db.Pool, actor uuid.UUID, p sqlc.AdminUpsertAbilityCatalogueEntryParams) (sqlc.AbilityCatalogue, error) {
	var out sqlc.AbilityCatalogue
	err := app.InUserTx(ctx, actor, func(tx pgx.Tx) error {
		var err error
		out, err = sqlc.New(tx).AdminUpsertAbilityCatalogueEntry(ctx, p)
		return err
	})
	return out, err
}

func c1Code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func c1World(t *testing.T) (context.Context, *db.Pool, *db.Pool, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	app := startPostgres(t)
	adm := connectAdmin(t, app)
	t.Cleanup(adm.Close)
	root := seedUserRow(t, adm, "c1-root-"+uuid.NewString()[:8]+"@example.test")
	markSuperadmin(t, adm, root)
	return ctx, app, adm, root
}

// TestAbilityCatalogueC1VersionCmpParity: the SQL ordering matches Go's
// wpversion.Compare over the same fixture internal/wpversion replays.
func TestAbilityCatalogueC1VersionCmpParity(t *testing.T) {
	ctx := context.Background()
	app := startPostgres(t)
	raw, err := os.ReadFile("../db/testdata/wpmgr_version_cmp_cases.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f struct {
		Cases [][3]json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Cases) == 0 {
		t.Fatalf("fixture empty or unreadable: %v", err)
	}
	for _, c := range f.Cases {
		var a, b string
		var want int
		if json.Unmarshal(c[0], &a) != nil || json.Unmarshal(c[1], &b) != nil || json.Unmarshal(c[2], &want) != nil {
			t.Fatalf("bad case %s", c)
		}
		var got int
		if err := app.QueryRow(ctx, `SELECT wpmgr_version_cmp($1, $2)`, a, b).Scan(&got); err != nil {
			t.Fatalf("wpmgr_version_cmp(%q, %q): %v", a, b, err)
		}
		if got != want {
			t.Errorf("wpmgr_version_cmp(%q, %q) = %d, want %d", a, b, got, want)
		}
	}
}

// TestAbilityCatalogueC1RangeOverlap: one admitted version range per name at
// any version. A non-admitted row is outside the rule; promoting it into an
// overlap is refused too.
func TestAbilityCatalogueC1RangeOverlap(t *testing.T) {
	ctx, app, _, root := c1World(t)
	name := "c1-" + uuid.NewString()[:8] + "/read-thing"

	if _, err := c1Upsert(ctx, app, root, c1VendorRead(root, name, "1.0", "2.0")); err != nil {
		t.Fatalf("first range: %v", err)
	}
	_, err := c1Upsert(ctx, app, root, c1VendorRead(root, name, "2.0", "3.0"))
	if c1Code(err) != "23P01" || !strings.Contains(err.Error(), "ability_catalogue_range_overlap") {
		t.Fatalf("OVERLAP ADMITTED: [2.0,3.0] over [1.0,2.0] answered %v, want 23P01 ability_catalogue_range_overlap", err)
	}
	// 1.10 > 1.9 numerically: [1.10, ...] would sit inside [1.0, 2.0].
	if _, err := c1Upsert(ctx, app, root, c1VendorRead(root, name, "1.10", "1.10.5")); c1Code(err) != "23P01" {
		t.Fatalf("OVERLAP ADMITTED: [1.10,1.10.5] answered %v, want 23P01", err)
	}
	if _, err := c1Upsert(ctx, app, root, c1VendorRead(root, name, "2.0.1", "3.0")); err != nil {
		t.Fatalf("adjacent range refused: %v", err)
	}
	// Open upper bound overlaps everything above its start.
	if _, err := c1Upsert(ctx, app, root, c1VendorRead(root, name, "2.5", "")); c1Code(err) != "23P01" {
		t.Fatalf("OVERLAP ADMITTED: [2.5,) answered %v, want 23P01", err)
	}
	// Not admitted: outside the rule. Promoting it is refused.
	held := c1VendorRead(root, name, "2.5", "")
	held.Status = "detect_only"
	row, err := c1Upsert(ctx, app, root, held)
	if err != nil {
		t.Fatalf("detect_only row refused: %v", err)
	}
	promote := held
	promote.Status = "admitted"
	promote.EntryID = pgtype.UUID{Bytes: row.EntryID, Valid: true}
	if _, err := c1Upsert(ctx, app, root, promote); c1Code(err) != "23P01" {
		t.Fatalf("OVERLAP ADMITTED BY UPDATE: promote answered %v, want 23P01", err)
	}
	// An inverted range is refused by the order CHECK.
	if _, err := c1Upsert(ctx, app, root, c1VendorRead(root, "c1-"+uuid.NewString()[:8]+"/x", "1.10", "1.9")); c1Code(err) != "23514" {
		t.Fatalf("INVERTED RANGE: [1.10,1.9] answered %v, want 23514", err)
	}
}

// TestAbilityCatalogueC1RangeOverlapUnderLock: a second writer of the same
// name waits on the per-name lock while the first is uncommitted, then sees
// its row and is refused. Without the lock it would pass the check against a
// snapshot that cannot see the first row, and both would commit.
func TestAbilityCatalogueC1RangeOverlapUnderLock(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	name := "c1-" + uuid.NewString()[:8] + "/race"

	firstIn := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- app.InUserTx(ctx, root, func(tx pgx.Tx) error {
			if _, err := sqlc.New(tx).AdminUpsertAbilityCatalogueEntry(ctx, c1VendorRead(root, name, "1.0", "2.0")); err != nil {
				close(firstIn)
				return err
			}
			close(firstIn)
			<-release
			return nil
		})
	}()
	<-firstIn

	secondDone := make(chan error, 1)
	go func() {
		_, err := c1Upsert(ctx, app, root, c1VendorRead(root, name, "1.5", "3.0"))
		secondDone <- err
	}()

	// Bounded: the second writer must be seen waiting on an advisory lock.
	waiting := false
	for i := 0; i < 50 && !waiting; i++ {
		var n int
		if err := adm.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND wait_event = 'advisory'
			  AND query LIKE '%admin_upsert_ability_catalogue_entry%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		waiting = n > 0
		if !waiting {
			select {
			case err := <-secondDone:
				close(release)
				t.Fatalf("LOCK MISSING: the second writer finished (%v) while the first was uncommitted", err)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first writer: %v", err)
	}
	if !waiting {
		t.Fatal("LOCK MISSING: the second writer was never seen waiting on the advisory lock")
	}
	if err := <-secondDone; c1Code(err) != "23P01" {
		t.Fatalf("OVERLAP ADMITTED UNDER RACE: second writer answered %v, want 23P01", err)
	}
	var n int
	if err := adm.QueryRow(ctx, `SELECT count(*) FROM ability_catalogue WHERE name = $1 AND status = 'admitted'`, name).Scan(&n); err != nil || n != 1 {
		t.Fatalf("admitted rows for %s = %d (err %v), want 1", name, n, err)
	}
}

// TestAbilityCatalogueC1RowRules: the per-row C1 CHECKs, and the app role
// still cannot write the table directly.
func TestAbilityCatalogueC1RowRules(t *testing.T) {
	ctx, app, _, root := c1World(t)
	uniq := func(s string) string { return "c1-" + uuid.NewString()[:8] + "/" + s }

	noHash := c1VendorRead(root, uniq("nohash"), "1.0", "2.0")
	noHash.SchemaStructSha256 = nil
	if _, err := c1Upsert(ctx, app, root, noHash); c1Code(err) != "23514" {
		t.Fatalf("UNPINNED SCHEMA: vendor row with no schema hash answered %v, want 23514", err)
	}
	noOut := c1VendorRead(root, uniq("noout"), "1.0", "2.0")
	noOut.OutputFields = nil
	if _, err := c1Upsert(ctx, app, root, noOut); c1Code(err) != "23514" {
		t.Fatalf("UNPINNED OUTPUT: vendor read with no output_fields answered %v, want 23514", err)
	}
	for _, bad := range []string{`{"fields":{"a":"float"}}`, `{"fields":{"a":"int"},"items":"int"}`, `["int"]`, `{"items":{"items":{"items":{"items":{"items":{"items":{"items":{"items":{"items":{"items":"int"}}}}}}}}}}`} {
		p := c1VendorRead(root, uniq("badshape"), "1.0", "2.0")
		p.OutputFields = []byte(bad)
		if _, err := c1Upsert(ctx, app, root, p); c1Code(err) != "23514" {
			t.Fatalf("BAD SHAPE %s answered %v, want 23514", bad, err)
		}
	}
	// A denied non-wpmgr entry never runs, so it pins nothing.
	denied := c1VendorRead(root, uniq("denied"), "", "")
	denied.Source, denied.Class, denied.Status = "core", "denied", "detect_only"
	denied.OwnerDir, denied.SchemaStructSha256, denied.OutputFields = nil, nil, nil
	if _, err := c1Upsert(ctx, app, root, denied); err != nil {
		t.Fatalf("denied core row refused: %v", err)
	}
	// WPMgr's own read pins nothing either: its code ships in the agent.
	own := c1VendorRead(root, "wpmgr-c1-"+uuid.NewString()[:8]+"/x", "", "")
	own.Source, own.Status = "wpmgr", "detect_only"
	own.OwnerDir, own.SchemaStructSha256, own.OutputFields = nil, nil, nil
	if _, err := c1Upsert(ctx, app, root, own); err != nil {
		t.Fatalf("wpmgr own read refused: %v", err)
	}

	// The app role has no direct write, definer or not.
	err := app.InUserTx(ctx, root, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO ability_catalogue (name, source, class, status, approval_mode, title, description)
			VALUES ($1, 'wpmgr', 'read', 'detect_only', 'none', 't', 'd')`, uniq("direct"))
		return err
	})
	if c1Code(err) != "42501" {
		t.Fatalf("DIRECT WRITE: app-role INSERT answered %v, want 42501", err)
	}
}

// TestAbilityCatalogueC1SeedAndRestamp: the three core abilities are seeded
// denied with our copy, and the boot stamp re-fills WPMgr's own entry hashes
// over bytes that now end in output_fields.
func TestAbilityCatalogueC1SeedAndRestamp(t *testing.T) {
	ctx, app, adm, _ := c1World(t)
	var n int
	if err := adm.QueryRow(ctx, `SELECT count(*) FROM ability_catalogue
		WHERE name IN ('core/get-site-info', 'core/get-environment-info', 'core/get-user-info')
		  AND source = 'core' AND class = 'denied' AND admission ? 'denied_reason'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("core denials seeded = %d (err %v), want 3", n, err)
	}
	if _, err := abilities.StampOwnEntryHashes(ctx, app, slog.Default()); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	var rows []sqlc.AbilityCatalogue
	if err := app.InAgentTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlc.New(tx).ListAdmittedAbilityCatalogue(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	own := 0
	for _, r := range rows {
		if r.Source != "wpmgr" {
			continue
		}
		own++
		b, sum, err := abilities.SendableEntry(r)
		if err != nil || r.EntrySha256 == nil || *r.EntrySha256 != sum {
			t.Fatalf("%s: stored hash does not match the sendable bytes (err %v)", r.Name, err)
		}
		if !bytes.HasSuffix(b, []byte(`,"output_fields":null}`)) {
			t.Fatalf("%s: entry bytes do not end in output_fields: %s", r.Name, b)
		}
	}
	if own == 0 {
		t.Fatal("no admitted wpmgr entries found; the seed is missing")
	}
}
