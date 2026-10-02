// m161 proofs (engine E3, D2): rest_route_catalogue is readable and not
// writable by the app role, its row CHECKs refuse a write route without an
// undo, the superadmin and stamp definers behave as documented, a REST write
// request cannot exist without its route facts, the request's route facts
// stay inside the RESTRICTIVE site scope, and the reservation refuses a row
// whose route moved after approval.
//
// Every call runs as wpmgr_app through the production dispatch (InUserTx,
// InTenantTx, RunTenantTx) and the generated sqlc method where one exists,
// so the statement proven is the statement shipped. Each test starts one
// container and seeds a handful of rows.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

var errM161Rollback = errors.New("m161: roll back")

func m161Constraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// m161Route is an honest write route; callers plant one fault.
func m161Route(actor uuid.UUID, id string) sqlc.AdminUpsertRestRouteParams {
	return sqlc.AdminUpsertRestRouteParams{
		ActorUserID:        actor,
		CreateRoute:        true,
		RouteID:            id,
		Method:             "POST",
		Namespace:          "wp/v2",
		Template:           "/wp/v2/pages/{id}",
		CorePattern:        `/wp/v2/pages/(?P<id>[\d]+)`,
		PathParams:         []byte(`{"id":{"type":"int","min":1,"max":9999999999,"required":true}}`),
		QueryKeys:          []byte(`{}`),
		PinnedQuery:        []byte(`{}`),
		BodyKeys:           []byte(`{"title":{"type":"string","max_len":200}}`),
		Class:              "write",
		OutputFields:       []byte(`{"fields":{"id":"int","title":{"fields":{"rendered":"string"}}}}`),
		Snapshot:           "post_fields",
		Target:             []byte(`{"kind":"post","param":"id","post_type":"page"}`),
		ArgRender:          []byte(`{}`),
		OperatorPermission: acprStr("site.content.edit"),
		EffectCopy:         "live",
		Enabled:            true,
		Title:              "Change a page title",
		Description:        "Test route.",
	}
}

func m161Upsert(ctx context.Context, app *db.Pool, actor uuid.UUID, p sqlc.AdminUpsertRestRouteParams) (sqlc.RestRouteCatalogue, error) {
	var out sqlc.RestRouteCatalogue
	err := app.InUserTx(ctx, actor, func(tx pgx.Tx) error {
		var err error
		out, err = sqlc.New(tx).AdminUpsertRestRoute(ctx, p)
		return err
	})
	return out, err
}

func m161Stamp(ctx context.Context, app *db.Pool, actor uuid.UUID, route, sha string) (sqlc.RestRouteCatalogue, error) {
	var out sqlc.RestRouteCatalogue
	err := app.InUserTx(ctx, actor, func(tx pgx.Tx) error {
		var err error
		out, err = sqlc.New(tx).StampWpmgrRestRouteHash(ctx, sqlc.StampWpmgrRestRouteHashParams{RouteID: route, RouteSha256: sha})
		return err
	})
	return out, err
}

func m161Audit(t *testing.T, ctx context.Context, app *db.Pool, actor uuid.UUID, route string) []sqlc.RestRouteCatalogueAudit {
	t.Helper()
	var rows []sqlc.RestRouteCatalogueAudit
	if err := app.InUserTx(ctx, actor, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlc.New(tx).ListRestRouteAudit(ctx, sqlc.ListRestRouteAuditParams{RouteID: route, RowLimit: 50})
		return err
	}); err != nil {
		t.Fatalf("read route audit %s: %v", route, err)
	}
	return rows
}

// m161RestWriteParams is an honest pending rest-write request for one site.
func m161RestWriteParams(tenant, site, grant, entry uuid.UUID, entrySha, routeSha, seed string) sqlc.InsertAbilityRequestParams {
	input := `{"route_id":"wp-v2-pages-update-fields","path":{"id":412},"query":{},"body":{"title":"Spring sale ` + seed + `"}}`
	sum := sha256.Sum256([]byte(input))
	post := int64(412)
	return sqlc.InsertAbilityRequestParams{
		TenantID:           tenant,
		SiteID:             site,
		ProposedByGrantID:  grant,
		EntryID:            entry,
		EntrySha256:        entrySha,
		AbilityName:        "wpmgr/rest-write",
		OperatorPermission: "site.content.edit",
		InputJson:          input,
		InputSha256:        hex.EncodeToString(sum[:]),
		TargetPostID:       &post,
		PrecheckDigest:     acprHex("precheck-" + seed),
		BaseFingerprint:    acprHex("fp-" + seed),
		SiteLabel:          "Shop",
		SiteHost:           "shop.example.com",
		GrantLabel:         "Laptop",
		GrantVia:           "token",
		TitleExcerpt:       acprStr("Spring sale"),
		PostType:           acprStr("page"),
		EffectCopy:         "live",
		Snapshot:           "post_fields",
		CardCopyVersion:    1,
		DigestNonce:        acprHex("nonce-" + seed),
		PresentedDigest:    acprHex("digest-" + seed),
		ExpiresAt:          time.Now().UTC().Add(24 * time.Hour),
		RouteID:            acprStr("wp-v2-pages-update-fields"),
		RouteSha256:        acprStr(routeSha),
		CardFacts:          []byte(`{"route_title":"Change a page's title or excerpt","method":"POST","changes":[{"key":"title","before":"Old","after":"Spring sale"}]}`),
	}
}

// TestRestRouteM161AppCannotWrite: wpmgr_app reads the seeded routes and
// holds no write on the catalogue or its audit; every direct write is refused
// 42501; the definers are SECURITY DEFINER with a pinned search_path and
// executable by wpmgr_app only; a non-superadmin upsert is refused.
//
// Mutations, one per assertion block:
//   - delete the m161 REVOKE line from the harness in rls_integration_test.go
//     -> "PRIVILEGE" goes red (has_table_privilege INSERT true).
//   - delete the superadmin IF block in admin_upsert_rest_route
//     -> "NON-SUPERADMIN WROTE" goes red.
func TestRestRouteM161AppCannotWrite(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	plain := seedUserRow(t, adm, "m161-plain-"+uuid.NewString()[:8]+"@example.test")

	var seeded []sqlc.RestRouteCatalogue
	if err := app.InUserTx(ctx, root, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InUserTx (m161 read)")
		var err error
		seeded, err = sqlc.New(tx).ListEnabledRestRoutes(ctx)
		return err
	}); err != nil {
		t.Fatalf("POSITIVE CONTROL: wpmgr_app cannot read rest_route_catalogue: %v", err)
	}
	want := map[string]string{
		"wp-v2-pages-list": "read", "wp-v2-posts-list": "read", "wp-v2-pages-get": "read",
		"wp-v2-posts-get": "read", "wp-v2-categories-list": "read", "wp-v2-tags-list": "read",
		"wp-v2-media-list": "read", "wp-v2-types": "read", "wp-v2-taxonomies": "read",
		"wp-v2-pages-update-fields": "write", "wp-v2-posts-update-fields": "write",
	}
	got := map[string]string{}
	for _, r := range seeded {
		got[r.RouteID] = r.Class
		if r.RouteSha256 != nil {
			t.Fatalf("SEED: route %s carries a route_sha256 before any stamp", r.RouteID)
		}
		if strings.Contains(string(r.PinnedQuery), `"edit"`) {
			t.Fatalf("SEED: route %s pins context=edit (owner ruling 5)", r.RouteID)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("SEED: %d routes seeded, want %d: %v", len(got), len(want), got)
	}
	for id, class := range want {
		if got[id] != class {
			t.Fatalf("SEED: route %s has class %q, want %q", id, got[id], class)
		}
	}

	for _, stmt := range []string{
		`INSERT INTO rest_route_catalogue (route_id, method, namespace, template, core_pattern, class, output_fields, title, description)
		 VALUES ('m161-smuggle', 'GET', 'wp/v2', '/wp/v2/users', '/wp/v2/users', 'read', '"string"', 'x', 'x')`,
		`UPDATE rest_route_catalogue SET enabled = false WHERE route_id = 'wp-v2-pages-list'`,
		`DELETE FROM rest_route_catalogue WHERE route_id = 'wp-v2-pages-list'`,
		`INSERT INTO rest_route_catalogue_audit (route_id, action, actor_user_id, after_row_sha256, after_enabled)
		 VALUES ('wp-v2-pages-list', 'insert', gen_random_uuid(), repeat('a', 64), true)`,
		`DELETE FROM rest_route_catalogue_audit`,
	} {
		err := app.InUserTx(ctx, root, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		if c1Code(err) != "42501" {
			t.Fatalf("DIRECT WRITE: %q answered %v, want 42501", stmt, err)
		}
	}

	var ins, upd, del, auditIns bool
	if err := adm.QueryRow(ctx, `SELECT
			has_table_privilege('wpmgr_app', 'public.rest_route_catalogue', 'INSERT'),
			has_table_privilege('wpmgr_app', 'public.rest_route_catalogue', 'UPDATE'),
			has_table_privilege('wpmgr_app', 'public.rest_route_catalogue', 'DELETE'),
			has_table_privilege('wpmgr_app', 'public.rest_route_catalogue_audit', 'INSERT')`).
		Scan(&ins, &upd, &del, &auditIns); err != nil {
		t.Fatalf("read table privileges: %v", err)
	}
	if ins || upd || del || auditIns {
		t.Fatalf("PRIVILEGE: wpmgr_app holds INSERT=%t UPDATE=%t DELETE=%t on the catalogue, INSERT=%t on the audit; want all false",
			ins, upd, del, auditIns)
	}

	for _, fn := range []string{
		"public.admin_upsert_rest_route(uuid, boolean, text, text, text, text, text, jsonb, jsonb, jsonb, jsonb, text, jsonb, text, jsonb, jsonb, text, text, boolean, text, text, text, text)",
		"public.stamp_wpmgr_rest_route_hash(text, text)",
	} {
		var appExec, pubExec, definer bool
		if err := adm.QueryRow(ctx, `SELECT
				has_function_privilege('wpmgr_app', $1, 'EXECUTE'),
				has_function_privilege('public', $1, 'EXECUTE'),
				(SELECT prosecdef AND proconfig @> ARRAY['search_path=public, pg_temp']
				   FROM pg_proc WHERE oid = $1::regprocedure)`, fn).
			Scan(&appExec, &pubExec, &definer); err != nil {
			t.Fatalf("read function privileges for %s: %v", fn, err)
		}
		if !appExec || pubExec || !definer {
			t.Fatalf("FUNCTION %s: wpmgr_app EXECUTE=%t (want true), PUBLIC EXECUTE=%t (want false), definer with pinned search_path=%t (want true)",
				fn, appExec, pubExec, definer)
		}
	}

	if _, err := m161Upsert(ctx, app, plain, m161Route(plain, "m161-plain")); c1Code(err) != "42501" {
		t.Fatalf("NON-SUPERADMIN WROTE: a plain user's upsert answered %v, want 42501", err)
	}
	if _, err := m161Upsert(ctx, app, root, m161Route(root, "m161-root")); err != nil {
		t.Fatalf("POSITIVE CONTROL: the superadmin upsert failed: %v", err)
	}
}

// TestRestRouteM161RowRules: the row CHECKs refuse each planted fault through
// the superadmin definer, the honest row lands with an audit row, create and
// update cannot stand in for each other, and the stamp moves NULL to a hash
// exactly once with a NULL-actor audit row.
//
// Mutations: ALTER TABLE rest_route_catalogue DROP CONSTRAINT
// rest_route_catalogue_write_rules_check -> "WRITE WITHOUT SNAPSHOT" goes red;
// DROP CONSTRAINT rest_route_catalogue_write_pins_check -> "WRITE PINS STATUS"
// goes red.
func TestRestRouteM161RowRules(t *testing.T) {
	ctx, app, _, root := c1World(t)

	cases := []struct {
		label, constraint string
		plant             func(p *sqlc.AdminUpsertRestRouteParams)
	}{
		{"WRITE WITHOUT SNAPSHOT", "rest_route_catalogue_write_rules_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.Snapshot = "none" }},
		{"WRITE WITHOUT TARGET", "rest_route_catalogue_write_rules_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.Target = nil }},
		{"WRITE WITHOUT PERMISSION", "rest_route_catalogue_write_rules_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.OperatorPermission = nil }},
		{"TARGET PARAM NOT A PATH PARAM", "rest_route_catalogue_target_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.Target = []byte(`{"kind":"post","param":"post_id"}`) }},
		{"TARGET WITHOUT PARAM", "rest_route_catalogue_target_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.Target = []byte(`{"kind":"post"}`) }},
		{"READ OVER POST", "rest_route_catalogue_read_is_get_check",
			func(p *sqlc.AdminUpsertRestRouteParams) {
				p.Class, p.Snapshot, p.EffectCopy, p.Target, p.BodyKeys = "read", "none", "none", nil, []byte(`{}`)
			}},
		{"CALLER-SUPPLIED STATUS", "rest_route_catalogue_forbidden_keys_check",
			func(p *sqlc.AdminUpsertRestRouteParams) {
				p.QueryKeys = []byte(`{"status":{"type":"string","max_len":10}}`)
			}},
		{"PINNED CONTEXT EDIT", "rest_route_catalogue_published_only_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.PinnedQuery = []byte(`{"context":"edit"}`) }},
		{"PINNED AND CALLER KEY", "rest_route_catalogue_keys_disjoint_check",
			func(p *sqlc.AdminUpsertRestRouteParams) {
				p.PinnedQuery = []byte(`{"lang":"en"}`)
				p.BodyKeys = []byte(`{"lang":{"type":"string","max_len":5}}`)
			}},
		{"BAD OUTPUT SHAPE", "rest_route_catalogue_output_fields_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.OutputFields = []byte(`{"fields":{"id":"float"}}`) }},
		{"UNTYPED BODY KEY", "rest_route_catalogue_body_keys_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.BodyKeys = []byte(`{"title":{"type":"blob"}}`) }},
		{"TEMPLATE OUTSIDE NAMESPACE", "rest_route_catalogue_template_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.Template = "/wp-abilities/v1/run" }},
		{"WRITE PINS STATUS", "rest_route_catalogue_write_pins_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.PinnedQuery = []byte(`{"status":"publish"}`) }},
		{"PINNED PATH PARAM", "rest_route_catalogue_keys_disjoint_check",
			func(p *sqlc.AdminUpsertRestRouteParams) {
				m161AsRead(p)
				p.PinnedQuery = []byte(`{"id":"5","context":"view"}`)
			}},
		{"STRING PATH PARAM", "rest_route_catalogue_path_params_int_check",
			func(p *sqlc.AdminUpsertRestRouteParams) {
				p.PathParams = []byte(`{"id":{"type":"string","max_len":20}}`)
			}},
		{"DOT-DOT TEMPLATE", "rest_route_catalogue_template_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.Template = "/wp/v2/pages/../users/{id}" }},
		{"PLACEHOLDER NOT A PATH PARAM", "rest_route_catalogue_template_params_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.Template = "/wp/v2/pages/{post}" }},
		{"PATH PARAM NOT IN TEMPLATE", "rest_route_catalogue_template_params_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.Template = "/wp/v2/pages" }},
		{"STRAY BRACE", "rest_route_catalogue_template_params_check",
			func(p *sqlc.AdminUpsertRestRouteParams) { p.Template = "/wp/v2/pages/{id}/{" }},
	}
	for i, c := range cases {
		p := m161Route(root, "m161-fault-"+string(rune('a'+i)))
		c.plant(&p)
		_, err := m161Upsert(ctx, app, root, p)
		if c1Code(err) != "23514" || m161Constraint(err) != c.constraint {
			t.Fatalf("%s: answered %v (constraint %q), want 23514 on %s", c.label, err, m161Constraint(err), c.constraint)
		}
	}

	// Positive control: the honest write row and the honest read row land.
	row, err := m161Upsert(ctx, app, root, m161Route(root, "m161-honest"))
	if err != nil {
		t.Fatalf("POSITIVE CONTROL: the honest write route was refused: %v", err)
	}
	read := m161Route(root, "m161-honest-read")
	m161AsRead(&read)
	read.PinnedQuery = []byte(`{"status":"publish","context":"view"}`)
	if _, err := m161Upsert(ctx, app, root, read); err != nil {
		t.Fatalf("POSITIVE CONTROL: the honest read route was refused: %v", err)
	}
	if a := m161Audit(t, ctx, app, root, row.RouteID); len(a) != 1 || a[0].Action != "insert" ||
		!a[0].ActorUserID.Valid || a[0].BeforeRowSha256 != nil {
		t.Fatalf("AUDIT: the insert wrote %+v, want one insert row naming its actor", a)
	}

	if _, err := m161Upsert(ctx, app, root, m161Route(root, "m161-honest")); c1Code(err) != "23505" {
		t.Fatalf("CREATE OVER EXISTING: answered %v, want 23505", err)
	}
	missing := m161Route(root, "m161-missing")
	missing.CreateRoute = false
	if _, err := m161Upsert(ctx, app, root, missing); c1Code(err) != "P0002" {
		t.Fatalf("UPDATE OF MISSING: answered %v, want P0002", err)
	}

	// The stamp: malformed, missing, honest, then already stamped.
	sha := acprHex("m161-route")
	if _, err := m161Stamp(ctx, app, root, row.RouteID, "NOT-HEX"); c1Code(err) != "22023" {
		t.Fatalf("STAMP MALFORMED: answered %v, want 22023", err)
	}
	if _, err := m161Stamp(ctx, app, root, "m161-nope", sha); c1Code(err) != "P0002" {
		t.Fatalf("STAMP MISSING: answered %v, want P0002", err)
	}
	stamped, err := m161Stamp(ctx, app, root, row.RouteID, sha)
	if err != nil || stamped.RouteSha256 == nil || *stamped.RouteSha256 != sha {
		t.Fatalf("STAMP: answered %+v, %v; want the hash stored", stamped.RouteSha256, err)
	}
	if _, err := m161Stamp(ctx, app, root, row.RouteID, acprHex("other")); c1Code(err) != "55000" {
		t.Fatalf("STAMP TWICE: answered %v, want 55000", err)
	}
	a := m161Audit(t, ctx, app, root, row.RouteID)
	if len(a) != 2 || a[0].ActorUserID.Valid || a[0].Action != "update" || a[0].BeforeRouteSha256 != nil ||
		a[0].AfterRouteSha256 == nil || *a[0].AfterRouteSha256 != sha {
		t.Fatalf("STAMP AUDIT: %+v, want a NULL-actor update from no hash to the stamped hash on top", a)
	}

	// An edit cannot keep the hash: content changed with the stored hash is
	// refused and changes nothing; the kill switch alone may keep it; a
	// content change with a NULL hash (re-stamp) lands.
	//
	// Mutation: delete the rest_route_catalogue_hash_not_moved IF block in
	// admin_upsert_rest_route -> "EDIT KEPT THE HASH" goes red.
	keep := m161EditOf(root, stamped)
	keep.BodyKeys = []byte(`{"title":{"type":"string","max_len":120}}`)
	_, err = m161Upsert(ctx, app, root, keep)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22023" || pgErr.Message != "rest_route_catalogue_hash_not_moved" {
		t.Fatalf("EDIT KEPT THE HASH: a content edit keeping route_sha256 answered %v, want 22023 rest_route_catalogue_hash_not_moved", err)
	}
	if cur := m161GetRoute(t, ctx, app, root, row.RouteID); string(cur.BodyKeys) == string(keep.BodyKeys) {
		t.Fatalf("EDIT KEPT THE HASH: the refused edit was stored")
	}
	off := m161EditOf(root, stamped)
	off.Enabled = false
	if _, err := m161Upsert(ctx, app, root, off); err != nil {
		t.Fatalf("OVER-FIRING: the kill switch alone (hash unchanged) was refused: %v", err)
	}
	restamp := m161EditOf(root, m161GetRoute(t, ctx, app, root, row.RouteID))
	restamp.BodyKeys = keep.BodyKeys
	restamp.RouteSha256 = nil
	if got, err := m161Upsert(ctx, app, root, restamp); err != nil || got.RouteSha256 != nil {
		t.Fatalf("OVER-FIRING: a content edit clearing the hash answered %v (hash %v), want success and NULL", err, got.RouteSha256)
	}
}

// m161AsRead turns the honest write route into an honest read route.
func m161AsRead(p *sqlc.AdminUpsertRestRouteParams) {
	p.Method, p.Class, p.Snapshot, p.EffectCopy, p.Target, p.BodyKeys, p.OperatorPermission =
		"GET", "read", "none", "none", nil, []byte(`{}`), nil
}

// TestRestRouteM161RequestRouteFacts: a rest-write request cannot be written
// without its route, a route cannot ride on another ability, the route and
// its hash travel together, a rest-write needs its card, and the facts cannot
// be updated by the app role.
//
// Mutation: ALTER TABLE assistant_ability_requests DROP CONSTRAINT
// assistant_ability_requests_rest_write_route_check -> "REST WRITE WITHOUT
// ROUTE" goes red.
func TestRestRouteM161RequestRouteFacts(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "m161-f-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	p := acprSitePrincipal(tenant, site)

	try := func(arg sqlc.InsertAbilityRequestParams) error {
		return pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (m161 insert)")
			_, err := sqlc.New(tx).InsertAbilityRequest(ctx, arg)
			return err
		})
	}
	honest := func(seed string) sqlc.InsertAbilityRequestParams {
		return m161RestWriteParams(tenant, site, uuid.New(), uuid.New(), acprHex("entry"), acprHex("route"), seed)
	}

	cases := []struct {
		label, constraint string
		arg               sqlc.InsertAbilityRequestParams
	}{
		{"REST WRITE WITHOUT ROUTE", "assistant_ability_requests_rest_write_route_check", func() sqlc.InsertAbilityRequestParams {
			a := honest("no-route")
			a.RouteID, a.RouteSha256 = nil, nil
			return a
		}()},
		{"ROUTE ON PAGE-CREATE", "assistant_ability_requests_rest_write_route_check", func() sqlc.InsertAbilityRequestParams {
			a := aarParams(tenant, site, uuid.New(), "route-on-create")
			a.RouteID, a.RouteSha256 = acprStr("wp-v2-pages-update-fields"), acprStr(acprHex("route"))
			return a
		}()},
		{"ROUTE WITHOUT HASH", "assistant_ability_requests_route_pair_check", func() sqlc.InsertAbilityRequestParams {
			a := honest("no-hash")
			a.RouteSha256 = nil
			return a
		}()},
		{"REST WRITE WITHOUT CARD", "assistant_ability_requests_rest_write_card_check", func() sqlc.InsertAbilityRequestParams {
			a := honest("no-card")
			a.CardFacts = nil
			return a
		}()},
		{"CARD NOT AN OBJECT", "assistant_ability_requests_card_facts_check", func() sqlc.InsertAbilityRequestParams {
			a := honest("card-array")
			a.CardFacts = []byte(`["x"]`)
			return a
		}()},
	}
	for _, c := range cases {
		err := try(c.arg)
		if c1Code(err) != "23514" || m161Constraint(err) != c.constraint {
			t.Fatalf("%s: answered %v (constraint %q), want 23514 on %s", c.label, err, m161Constraint(err), c.constraint)
		}
	}

	// Positive controls: the honest rest-write and an unchanged page-create.
	row := aarInsert(t, pool, p, honest("honest"))
	if row.RouteID == nil || *row.RouteID != "wp-v2-pages-update-fields" || len(row.CardFacts) == 0 {
		t.Fatalf("POSITIVE CONTROL: the honest rest-write stored route %v card %q", row.RouteID, row.CardFacts)
	}
	aarInsert(t, pool, p, aarParams(tenant, site, uuid.New(), "plain-create"))

	for _, stmt := range []string{
		`UPDATE assistant_ability_requests SET route_id = 'wp-v2-posts-update-fields' WHERE id = $1`,
		`UPDATE assistant_ability_requests SET route_sha256 = repeat('b', 64) WHERE id = $1`,
		`UPDATE assistant_ability_requests SET card_facts = '{}'::jsonb WHERE id = $1`,
	} {
		err := pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, row.ID)
			return err
		})
		if c1Code(err) != "42501" {
			t.Fatalf("FACT MOVED: %q answered %v, want 42501", stmt, err)
		}
	}
}

// TestRestRouteM161RequestSiteScope re-proves m156's site scope over the new
// columns with rest-write rows seeded as wpmgr_app: a principal scoped to S1
// reads none of S2's route facts, a foreign tenant reads none at all.
//
// Mutation: DROP POLICY assistant_ability_requests_site_scope ON
// assistant_ability_requests -> "SITE-SCOPE LEAK" goes red.
func TestRestRouteM161RequestSiteScope(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenantA := seedTenant(t, pool, "m161-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "m161-b-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenantA, "")
	s2 := seedSite(t, pool, tenantA, "")
	both := acprSitePrincipal(tenantA, s1, s2)

	r1 := aarInsert(t, pool, both, m161RestWriteParams(tenantA, s1, uuid.New(), uuid.New(), acprHex("e"), acprHex("r"), "s1"))
	r2 := aarInsert(t, pool, both, m161RestWriteParams(tenantA, s2, uuid.New(), uuid.New(), acprHex("e"), acprHex("r"), "s2"))

	countFacts := func(tx pgx.Tx, id uuid.UUID) int {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM assistant_ability_requests
			WHERE id = $1 AND route_id IS NOT NULL AND card_facts IS NOT NULL`, id).Scan(&n); err != nil {
			t.Fatalf("count route facts: %v", err)
		}
		return n
	}

	if err := pool.RunTenantTx(ctx, acprSitePrincipal(tenantA, s1), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m161 single site S1)")
		if n := countFacts(tx, r2.ID); n != 0 {
			t.Fatalf("SITE-SCOPE LEAK: a principal scoped to site %s reads the route facts of request %s on site %s", s1, r2.ID, s2)
		}
		if n := countFacts(tx, r1.ID); n != 1 {
			t.Fatalf("OVER-FIRING: a principal scoped to site %s cannot read its own request %s (%d rows)", s1, r1.ID, n)
		}
		return nil
	}); err != nil {
		t.Fatalf("single-site read: %v", err)
	}

	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m161 foreign tenant)")
		if n := countFacts(tx, r1.ID) + countFacts(tx, r2.ID); n != 0 {
			t.Fatalf("TENANCY LEAK: tenant B reads %d of tenant A's rest-write requests", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("foreign tenant read: %v", err)
	}
}

// m161EditOf builds a superadmin update that rewrites cur unchanged; callers
// change one field.
func m161EditOf(actor uuid.UUID, cur sqlc.RestRouteCatalogue) sqlc.AdminUpsertRestRouteParams {
	return sqlc.AdminUpsertRestRouteParams{
		ActorUserID: actor, CreateRoute: false, RouteID: cur.RouteID, Method: cur.Method,
		Namespace: cur.Namespace, Template: cur.Template, CorePattern: cur.CorePattern,
		PathParams: cur.PathParams, QueryKeys: cur.QueryKeys, PinnedQuery: cur.PinnedQuery,
		BodyKeys: cur.BodyKeys, Class: cur.Class, OutputFields: cur.OutputFields,
		Snapshot: cur.Snapshot, Target: cur.Target, ArgRender: cur.ArgRender,
		OperatorPermission: cur.OperatorPermission, EffectCopy: cur.EffectCopy,
		Enabled: cur.Enabled, MinWpVersion: cur.MinWpVersion, Title: cur.Title,
		Description: cur.Description, RouteSha256: cur.RouteSha256,
	}
}

func m161GetRoute(t *testing.T, ctx context.Context, app *db.Pool, actor uuid.UUID, id string) sqlc.RestRouteCatalogue {
	t.Helper()
	var cur sqlc.RestRouteCatalogue
	if err := app.InUserTx(ctx, actor, func(tx pgx.Tx) error {
		var err error
		cur, err = sqlc.New(tx).GetRestRoute(ctx, id)
		return err
	}); err != nil {
		t.Fatalf("read route %s: %v", id, err)
	}
	return cur
}

// m161Reserve runs the shipped dispatch read and reservation for one row in
// the tenant's transaction, then rolls back so the row stays approved.
func m161Reserve(t *testing.T, ctx context.Context, app *db.Pool, tenant, id uuid.UUID) (int64, sqlc.GetApprovedAbilityRequestForDispatchRow) {
	t.Helper()
	var n int64
	var got sqlc.GetApprovedAbilityRequestForDispatchRow
	if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m161 reserve)")
		q := sqlc.New(tx)
		var err error
		got, err = q.GetApprovedAbilityRequestForDispatch(ctx, sqlc.GetApprovedAbilityRequestForDispatchParams{TenantID: tenant, ID: id})
		if err != nil {
			return err
		}
		n, err = q.ReserveAbilityRequestForDispatch(ctx, sqlc.ReserveAbilityRequestForDispatchParams{TenantID: tenant, ID: id})
		if err != nil {
			return err
		}
		return errM161Rollback
	}); err != nil && !errors.Is(err, errM161Rollback) {
		t.Fatalf("reserve: %v", err)
	}
	return n, got
}

// m161DispatchWorld stamps the wpmgr/rest-write entry and the named routes,
// and returns the entry id and hash and each route's stamped hash.
func m161DispatchWorld(t *testing.T, ctx context.Context, app *db.Pool, root uuid.UUID, routes ...string) (uuid.UUID, string, map[string]string) {
	t.Helper()
	var entry uuid.UUID
	if err := app.InUserTx(ctx, root, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT entry_id FROM ability_catalogue WHERE name = 'wpmgr/rest-write'`).Scan(&entry)
	}); err != nil {
		t.Fatalf("INDETERMINATE: wpmgr/rest-write seed missing: %v", err)
	}
	entrySha := acprHex("m161-entry")
	if err := app.InUserTx(ctx, root, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).StampWpmgrAbilityEntryHash(ctx, sqlc.StampWpmgrAbilityEntryHashParams{EntryID: entry, EntrySha256: entrySha})
		return err
	}); err != nil {
		t.Fatalf("stamp entry: %v", err)
	}
	shas := map[string]string{}
	for _, r := range routes {
		shas[r] = acprHex("m161-route-" + r)
		if _, err := m161Stamp(ctx, app, root, r, shas[r]); err != nil {
			t.Fatalf("stamp route %s: %v", r, err)
		}
	}
	return entry, entrySha, shas
}

// m161ApprovedRequest inserts and approves one rest-write request naming
// route (with routeSha) on site.
func m161ApprovedRequest(t *testing.T, app *db.Pool, tenant, site, entry uuid.UUID, entrySha, route, routeSha, seed string) sqlc.AssistantAbilityRequest {
	t.Helper()
	arg := m161RestWriteParams(tenant, site, uuid.New(), entry, entrySha, routeSha, seed)
	arg.RouteID = acprStr(route)
	row := aarInsert(t, app, acprSitePrincipal(tenant, site), arg)
	if _, err := aarApprove(t, app, row, row.PresentedDigest); err != nil {
		t.Fatalf("approve %s: %v", seed, err)
	}
	return row
}

// TestRestRouteM161ReservationNeedsApprovedRoute: an approved rest-write
// against the stamped route is reservable; once a superadmin edits the route
// (its hash moves), the dispatch read reports route_hash_current false and
// the reservation reserves nothing.
//
// Mutation: delete "AND rr.route_sha256 = r.route_sha256" from
// ReserveAbilityRequestForDispatch and regenerate -> "RESERVED AGAINST A
// CHANGED ROUTE" goes red.
func TestRestRouteM161ReservationNeedsApprovedRoute(t *testing.T) {
	ctx, app, _, root := c1World(t)
	tenant := seedTenant(t, app, "m161-r-"+uuid.NewString()[:8])
	site := seedSite(t, app, tenant, "")
	const route = "wp-v2-pages-update-fields"
	entry, entrySha, shas := m161DispatchWorld(t, ctx, app, root, route)

	row := m161ApprovedRequest(t, app, tenant, site, entry, entrySha, route, shas[route], "reserve")
	if n, got := m161Reserve(t, ctx, app, tenant, row.ID); n != 1 || !got.RouteHashCurrent || !got.RouteEnabled || !got.EntryHashCurrent {
		t.Fatalf("POSITIVE CONTROL: the approved rest-write against the current route reserved %d rows (%+v), want 1 and all current", n, got)
	}

	edit := m161EditOf(root, m161GetRoute(t, ctx, app, root, route))
	edit.BodyKeys = []byte(`{"title":{"type":"string","max_len":100}}`)
	edit.RouteSha256 = acprStr(acprHex("m161-route-v2"))
	if _, err := m161Upsert(ctx, app, root, edit); err != nil {
		t.Fatalf("superadmin route edit: %v", err)
	}
	if n, got := m161Reserve(t, ctx, app, tenant, row.ID); n != 0 || got.RouteHashCurrent {
		t.Fatalf("RESERVED AGAINST A CHANGED ROUTE: reserved %d rows (route current=%t) after the route hash moved, want 0 and false", n, got.RouteHashCurrent)
	}
}

// TestRestRouteM161KillSwitchAndClass: a route disabled with its hash
// unchanged is not reserved and the dispatch read reports it not enabled
// (route_disabled); a request naming a read-class route is not reserved
// either, though its hash is current.
//
// Mutations:
//   - delete "AND rr.enabled" from ReserveAbilityRequestForDispatch and
//     regenerate -> "RESERVED ON A DISABLED ROUTE" goes red.
//   - delete "AND rr.class = 'write'" from ReserveAbilityRequestForDispatch
//     and regenerate -> "RESERVED ON A READ ROUTE" goes red.
func TestRestRouteM161KillSwitchAndClass(t *testing.T) {
	ctx, app, _, root := c1World(t)
	tenant := seedTenant(t, app, "m161-k-"+uuid.NewString()[:8])
	site := seedSite(t, app, tenant, "")
	const write, read = "wp-v2-pages-update-fields", "wp-v2-pages-get"
	entry, entrySha, shas := m161DispatchWorld(t, ctx, app, root, write, read)

	row := m161ApprovedRequest(t, app, tenant, site, entry, entrySha, write, shas[write], "kill")
	if n, _ := m161Reserve(t, ctx, app, tenant, row.ID); n != 1 {
		t.Fatalf("POSITIVE CONTROL: the approved rest-write reserved %d rows before the kill switch, want 1", n)
	}

	off := m161EditOf(root, m161GetRoute(t, ctx, app, root, write))
	off.Enabled = false
	if _, err := m161Upsert(ctx, app, root, off); err != nil {
		t.Fatalf("kill switch (enabled=false, hash unchanged) refused: %v", err)
	}
	if cur := m161GetRoute(t, ctx, app, root, write); cur.Enabled || cur.RouteSha256 == nil || *cur.RouteSha256 != shas[write] {
		t.Fatalf("INDETERMINATE: after the kill switch the route is enabled=%t hash=%v, want false and unchanged", cur.Enabled, cur.RouteSha256)
	}
	n, got := m161Reserve(t, ctx, app, tenant, row.ID)
	if n != 0 {
		t.Fatalf("RESERVED ON A DISABLED ROUTE: reserved %d rows, want 0", n)
	}
	if !got.RouteHashCurrent || got.RouteEnabled {
		t.Fatalf("DISPATCH READ: route_hash_current=%t route_enabled=%t on a disabled route, want true and false (route_disabled)", got.RouteHashCurrent, got.RouteEnabled)
	}

	readRow := m161ApprovedRequest(t, app, tenant, site, entry, entrySha, read, shas[read], "read-class")
	n, got = m161Reserve(t, ctx, app, tenant, readRow.ID)
	if n != 0 {
		t.Fatalf("RESERVED ON A READ ROUTE: a rest-write naming read route %s reserved %d rows, want 0", read, n)
	}
	if !got.RouteHashCurrent || got.RouteEnabled {
		t.Fatalf("DISPATCH READ: route_hash_current=%t route_enabled=%t on a read route, want true and false", got.RouteHashCurrent, got.RouteEnabled)
	}
}
