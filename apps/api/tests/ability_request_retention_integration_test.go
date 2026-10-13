package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const r01Migration = "20261013020000_m180_ability_request_content_retention.sql"
const r01Input = `{"outline_version":3,"design":{"colors":{"ground":"#F6F5F2","surface":"#FFFFFF","ink":"#15171C","muted":"#5A606C","accent":"#0D7A63","on_accent":"#FFFFFF"},"fonts":{"heading":"Manrope","body":"Source Sans 3"},"scale":{"base_px":17,"ratio":"major_third"},"radius":"md","density":"normal"},"title":"Private words","outline":[]}`

// Fixtures enter pending, then use the application's role and approval context.
func r01Request(t *testing.T, app *db.Pool, tenant, site uuid.UUID, state string, age time.Duration, undo bool, ability string, post int64) uuid.UUID {
	t.Helper()
	return r01RequestWithInput(t, app, tenant, site, state, age, undo, ability, post, r01Input)
}

func r01RequestWithInput(t *testing.T, app *db.Pool, tenant, site uuid.UUID, state string, age time.Duration, undo bool, ability string, post int64, input string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id, user := uuid.New(), uuid.New()
	finished := time.Now().Add(-age)
	err := app.InTenantTxAsUser(ctx, tenant, user, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "R-01 fixture")
		_, err := tx.Exec(ctx, `INSERT INTO assistant_ability_requests (
   id, tenant_id, site_id, proposed_by_grant_id, entry_id, entry_sha256, ability_name,
   operator_permission, input_json, input_sha256, precheck_digest, base_fingerprint,
   site_label, site_host, grant_label, grant_via, effect_copy, snapshot, card_copy_version,
   digest_nonce, presented_digest, state, created_at, expires_at, card_facts,
   route_id, route_sha256, target_post_id)
   VALUES ($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,repeat('a',64),$8::text,'site.content.edit',$6::text,
   encode(sha256(convert_to($6::text,'UTF8')),'hex'),repeat('b',64),'new:seed',
   'Shop','shop.example','Laptop','token','draft','created_post_trash',1,
   repeat('c',64),repeat('d',64),'pending',$7::timestamptz - interval '2 days',$7::timestamptz,
   '{"kind":"builder_edit"}'::jsonb,
   CASE WHEN $8::text = 'wpmgr/rest-write' THEN 'post' END,
   CASE WHEN $8::text = 'wpmgr/rest-write' THEN repeat('e',64) END,
   CASE WHEN $8::text = 'wpmgr/page-edit' THEN 418 END)`, id, tenant, site, uuid.New(), uuid.New(), input, finished, ability)
		if err != nil || state == "pending" {
			return err
		}
		switch state {
		case "expired":
			_, err = tx.Exec(ctx, `UPDATE assistant_ability_requests SET state='expired' WHERE id=$1::uuid`, id)
		case "withdrawn":
			_, err = tx.Exec(ctx, `UPDATE assistant_ability_requests SET state='withdrawn', withdrawn_at=$2::timestamptz WHERE id=$1::uuid`, id, finished)
		case "declined":
			_, err = tx.Exec(ctx, `UPDATE assistant_ability_requests SET state='declined', decided_at=$2::timestamptz, decided_by_user_id=$3::uuid WHERE id=$1::uuid`, id, finished, user)
		default:
			_, err = tx.Exec(ctx, `UPDATE assistant_ability_requests SET state='approved', decided_at=$2::timestamptz - interval '1 hour',
    decided_by_user_id=$3::uuid, dispatch_deadline_at=$2::timestamptz + interval '1 hour' WHERE id=$1::uuid`, id, finished, user)
			if err != nil {
				return err
			}
			outcome := "created"
			if ability != "wpmgr/page-create" {
				outcome = "applied"
			}
			if state == "failed" {
				outcome = "failed"
			}
			if state == "not_sent" {
				outcome = "not_sent"
			}
			if state == "outcome_unknown" {
				outcome = "outcome_unknown"
			}
			var result *string = &outcome
			if state == "reconciling" {
				state, result = "outcome_unknown", nil
			}
			_, err = tx.Exec(ctx, `UPDATE assistant_ability_requests SET state=$2::text, outcome=$3::text,
    outcome_at=CASE WHEN $3::text IS NOT NULL THEN $4::timestamptz END,
    claimed_at=CASE WHEN $2::text <> 'not_sent' THEN $4::timestamptz END,
    not_sent_reason=CASE WHEN $2::text='not_sent' THEN 'dispatch_deadline_passed' END,
    unknown_since=CASE WHEN $2::text='outcome_unknown' THEN $4::timestamptz END,
    created_post_id=CASE WHEN $2::text IN ('done','failed','outcome_unknown') THEN $5::bigint END WHERE id=$1::uuid`, id, state, result, finished, post)
		}
		if err != nil || !undo {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE assistant_ability_requests SET undo_state='in_progress', undo_available_until=$2::timestamptz + interval '1 day',
   undo_by_user_id=$3::uuid, undo_started_at=$2::timestamptz WHERE id=$1::uuid`, id, finished, user)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestR01AbilityRequestRetention(t *testing.T) {
	for _, single := range []bool{true, false} {
		t.Run(fmt.Sprintf("single_role_%t", single), func(t *testing.T) {
			app, owner := ds01Postgres(t, single, false)
			ctx := context.Background()
			body, err := migrations.FS.ReadFile(r01Migration)
			if err != nil {
				t.Fatal(err)
			}
			replace := func(from, to string) {
				if strings.Count(string(body), from) != 1 {
					t.Fatalf("retention mutation target is missing or ambiguous: %q", from)
				}
				body = []byte(strings.Replace(string(body), from, to, 1))
			}
			// Optional mutation runs prove that each retention guard can fail red.
			switch defect := os.Getenv("R01_RETENTION_DEFECT"); defect {
			case "":
			case "recent":
				replace("       OR v_finished >= statement_timestamp() - interval '15 days'\n", "")
			case "nonfinal":
				replace("        ELSE NULL", "        ELSE p_expires_at")
			case "missingdraft":
				replace("SET retained_draft = public.ability_request_retained_draft(r.ability_name, r.input_json)", "SET retained_draft = NULL")
				replace("       OR NEW.retained_draft IS DISTINCT FROM\n          public.ability_request_retained_draft(OLD.ability_name, OLD.input_json)\n", "")
			default:
				t.Fatalf("unknown retention defect %q", defect)
			}
			if err := pgx.BeginFunc(ctx, owner.Pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, string(body)); return err }); err != nil {
				t.Fatalf("reapply retention migration: %v", err)
			}
			for _, column := range []string{"input_json", "card_facts", "retained_draft", "content_cleared_at"} {
				var writable bool
				if err := app.QueryRow(ctx, `SELECT has_column_privilege(current_user,'public.assistant_ability_requests',$1::text,'UPDATE')`, column).Scan(&writable); err != nil {
					t.Fatal(err)
				}
				if writable != single {
					t.Fatalf("%s direct privilege=%t for single_role=%t", column, writable, single)
				}
			}
			tenant, site := ds01Seed(t, app)
			type fixture struct {
				name, state, ability string
				age                  time.Duration
				undo, cleared        bool
				id                   uuid.UUID
			}
			fixtures := []fixture{
				{name: "done draft", state: "done", ability: "wpmgr/page-create", age: 16 * 24 * time.Hour, cleared: true},
				{name: "failed", state: "failed", ability: "wpmgr/page-create", age: 16 * 24 * time.Hour, cleared: true},
				{name: "declined", state: "declined", ability: "wpmgr/page-create", age: 16 * 24 * time.Hour, cleared: true},
				{name: "expired", state: "expired", ability: "wpmgr/page-create", age: 16 * 24 * time.Hour, cleared: true},
				{name: "withdrawn", state: "withdrawn", ability: "wpmgr/page-create", age: 16 * 24 * time.Hour, cleared: true},
				{name: "not sent", state: "not_sent", ability: "wpmgr/page-create", age: 16 * 24 * time.Hour, cleared: true},
				{name: "REST write", state: "done", ability: "wpmgr/rest-write", age: 16 * 24 * time.Hour, cleared: true},
				{name: "page edit", state: "done", ability: "wpmgr/page-edit", age: 16 * 24 * time.Hour, cleared: true},
				{name: "pending", state: "pending", ability: "wpmgr/page-create", age: 20 * 24 * time.Hour},
				{name: "recent", state: "done", ability: "wpmgr/page-create", age: 14 * 24 * time.Hour},
				{name: "undo running", state: "done", ability: "wpmgr/page-create", age: 20 * 24 * time.Hour, undo: true},
				{name: "given up", state: "outcome_unknown", ability: "wpmgr/page-create", age: 20 * 24 * time.Hour, cleared: true},
				{name: "recent give up", state: "outcome_unknown", ability: "wpmgr/page-create", age: 14 * 24 * time.Hour},
				{name: "still reconciling", state: "reconciling", ability: "wpmgr/page-create", age: 20 * 24 * time.Hour},
			}
			for i := range fixtures {
				f := &fixtures[i]
				f.id = r01Request(t, app, tenant, site, f.state, f.age, f.undo, f.ability, int64(418+i))
				if f.name == "recent give up" {
					if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
						_, err := tx.Exec(ctx, `UPDATE assistant_ability_requests SET unknown_since=now()-interval '40 days' WHERE id=$1::uuid`, f.id)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, f := range fixtures {
				statements := []string{
					`UPDATE assistant_ability_requests SET input_json=input_json WHERE id=$1::uuid`,
					`UPDATE assistant_ability_requests SET card_facts=card_facts WHERE id=$1::uuid`,
					`UPDATE assistant_ability_requests SET retained_draft=retained_draft WHERE id=$1::uuid`,
					`UPDATE assistant_ability_requests SET content_cleared_at=content_cleared_at WHERE id=$1::uuid`,
					`UPDATE assistant_ability_requests SET input_json='{"replacement":true}' WHERE id=$1::uuid`,
					`UPDATE assistant_ability_requests SET card_facts='{}' WHERE id=$1::uuid`,
					`UPDATE assistant_ability_requests SET retained_draft='{}' WHERE id=$1::uuid`,
					`UPDATE assistant_ability_requests SET content_cleared_at=now() WHERE id=$1::uuid`,
					`UPDATE assistant_ability_requests SET input_json='{}',card_facts=NULL,retained_draft=public.ability_request_retained_draft(ability_name,input_json),content_cleared_at=now(),site_reported_text='changed' WHERE id=$1::uuid`,
				}
				if !f.cleared {
					statements = append(statements, `UPDATE assistant_ability_requests SET input_json='{}', card_facts=NULL,
      retained_draft=public.ability_request_retained_draft(ability_name,input_json), content_cleared_at=now() WHERE id=$1::uuid`)
				}
				for _, stmt := range statements {
					err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt, f.id); return err })
					if err == nil {
						t.Fatalf("%s: direct mutation allowed: %s", f.name, stmt)
					}
					var pgErr *pgconn.PgError
					if !errors.As(err, &pgErr) || (pgErr.Code != "42501" && pgErr.Code != "55000") {
						t.Fatalf("%s: unexpected error: %v", f.name, err)
					}
				}
			}
			if single {
				rollback := errors.New("rollback allowed transition")
				err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
					tag, err := tx.Exec(ctx, `UPDATE assistant_ability_requests SET input_json='{}',card_facts=NULL,
                        retained_draft=public.ability_request_retained_draft(ability_name,input_json),content_cleared_at=now() WHERE id=$1::uuid`, fixtures[0].id)
					if err != nil {
						return err
					}
					if tag.RowsAffected() != 1 {
						t.Fatal("allowed transition did not reach the row")
					}
					return rollback
				})
				if !errors.Is(err, rollback) {
					t.Fatalf("allowed transition: %v", err)
				}
			}
			// A batch returns its actual count and cannot exceed its requested size.
			n, err := sqlc.New(app.Pool).ClearAbilityRequestContent(ctx, 1)
			if err != nil || n != 1 {
				t.Fatalf("bounded first batch: n=%d err=%v", n, err)
			}
			worker := abilityrequest.NewContentRetentionWorker(app, nil)
			if err := worker.Work(ctx, nil); err != nil {
				t.Fatalf("retention job: %v", err)
			}
			if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
				q := sqlc.New(tx)
				for _, f := range fixtures {
					row, err := q.GetAbilityRequestForSite(ctx, sqlc.GetAbilityRequestForSiteParams{TenantID: tenant, ID: f.id, SiteID: site})
					if err != nil {
						return err
					}
					if row.ContentClearedAt.Valid != f.cleared {
						t.Fatalf("%s: cleared=%t", f.name, row.ContentClearedAt.Valid)
					}
					if !f.cleared {
						if row.InputJson != r01Input {
							t.Fatalf("%s input changed", f.name)
						}
						continue
					}
					if row.InputJson != "{}" || len(row.CardFacts) != 0 || row.InputSha256 != acprHex(r01Input) {
						t.Fatalf("%s: clearing did not preserve hash and remove content", f.name)
					}
					if f.ability == "wpmgr/page-create" {
						var retained map[string]json.RawMessage
						if err := json.Unmarshal(row.RetainedDraft, &retained); err != nil {
							t.Fatalf("%s: cleared page-create has no retained design: %v", f.name, err)
						}
						if len(retained) != 2 || string(retained["outline_version"]) != "3" || retained["design"] == nil {
							t.Fatalf("%s: retained draft=%s", f.name, row.RetainedDraft)
						}
					} else if len(row.RetainedDraft) != 0 {
						t.Fatalf("%s retained unrelated content", f.name)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// The same repository path used by page edits reads the retained design.
			got, err := mcp.NewRepo(app).EligibleDraftCreateInput(ctx, acprSitePrincipal(tenant, site), site, 418)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) == "{}" || !strings.Contains(string(got), `"design"`) || strings.Contains(string(got), "Private words") {
				t.Fatalf("cleared draft input = %s", got)
			}

			n, err = sqlc.New(app.Pool).ClearAbilityRequestContent(ctx, 1)
			if err != nil || n != 0 {
				t.Fatalf("repeat batch: n=%d err=%v", n, err)
			}
			for _, limit := range []int32{0, -1, 1001} {
				if _, err := sqlc.New(app.Pool).ClearAbilityRequestContent(ctx, limit); err == nil {
					t.Fatalf("invalid batch limit %d accepted", limit)
				}
			}
			for _, f := range fixtures {
				if !f.cleared {
					continue
				}
				for _, stmt := range []string{
					`UPDATE assistant_ability_requests SET input_json=$2::text,card_facts='{}',retained_draft=NULL,content_cleared_at=NULL WHERE id=$1::uuid`,
					`UPDATE assistant_ability_requests SET retained_draft='{}' WHERE id=$1::uuid AND $2::text <> ''`,
				} {
					err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt, f.id, r01Input); return err })
					var pgErr *pgconn.PgError
					if !errors.As(err, &pgErr) || (pgErr.Code != "42501" && pgErr.Code != "55000") {
						t.Fatalf("%s: unexpected cleared-content mutation result: %v", f.name, err)
					}
				}
			}
			if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
				var before, after string
				if err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.agent',true),'')`).Scan(&before); err != nil {
					return err
				}
				n, err := sqlc.New(tx).ClearAbilityRequestContent(ctx, 1)
				if err != nil || n != 0 {
					return fmt.Errorf("empty scoped batch: n=%d err=%v", n, err)
				}
				if err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.agent',true),'')`).Scan(&after); err != nil {
					return err
				}
				if before != after {
					t.Fatalf("retention changed agent context: %q -> %q", before, after)
				}
				tag, err := tx.Exec(ctx, `UPDATE assistant_ability_requests SET site_reported_text='record kept' WHERE id=$1::uuid`, fixtures[0].id)
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 1 {
					t.Fatal("ordinary record update was blocked")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

		})
	}
}

func TestR01RetentionTenantScopes(t *testing.T) {
	for _, single := range []bool{true, false} {
		t.Run(fmt.Sprintf("single_role_%t", single), func(t *testing.T) {
			app, owner := ds01Postgres(t, single, false)
			ctx := context.Background()
			var command, qualifier string
			var noWriteCheck bool
			if err := app.QueryRow(ctx, `SELECT cmd, qual, with_check IS NULL FROM pg_policies
				WHERE schemaname='public' AND tablename='assistant_ability_requests'
				AND policyname='assistant_ability_requests_agent'`).Scan(&command, &qualifier, &noWriteCheck); err != nil {
				t.Fatal(err)
			}
			if command != "SELECT" || !noWriteCheck || qualifier != "(current_setting('app.agent'::text, true) = 'on'::text)" {
				t.Fatalf("agent policy: command=%s qualifier=%s no_write_check=%t", command, qualifier, noWriteCheck)
			}
			tenant, site := ds01Seed(t, app)
			otherTenant, otherSite := ds01Seed(t, app)
			locked := r01Request(t, app, tenant, site, "done", 18*24*time.Hour, false, "wpmgr/page-create", 600)
			first := r01Request(t, app, tenant, site, "done", 16*24*time.Hour, false, "wpmgr/page-create", 601)
			second := r01Request(t, app, otherTenant, otherSite, "done", 16*24*time.Hour, false, "wpmgr/page-create", 602)

			if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
				mcpAssertAndReportRole(t, tx, "R-01 agent read and cross-tenant update")
				if _, err := tx.Exec(ctx, `SELECT set_config('app.agent','on',true), set_config('app.site_scope','off',true)`); err != nil {
					return err
				}
				var visible bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM assistant_ability_requests WHERE id=$1::uuid)`, second).Scan(&visible); err != nil {
					return err
				}
				if !visible {
					t.Fatal("agent read did not reach the other tenant's row")
				}
				tag, err := tx.Exec(ctx, `UPDATE assistant_ability_requests SET site_reported_text='agent write' WHERE id=$1::uuid`, second)
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 0 {
					t.Fatal("agent context wrote another tenant's request")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			readSettings := func(tx pgx.Tx) [3]string {
				t.Helper()
				var settings [3]string
				if err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.agent',true),''),
					coalesce(current_setting('app.tenant_id',true),''), coalesce(current_setting('app.site_scope',true),'')`).
					Scan(&settings[0], &settings[1], &settings[2]); err != nil {
					t.Fatal(err)
				}
				return settings
			}
			clearScoped := func(limit int32) {
				t.Helper()
				if err := app.RunTenantTx(ctx, acprSitePrincipal(tenant, site), func(tx pgx.Tx) error {
					mcpAssertAndReportRole(t, tx, "R-01 scoped retention")
					if _, err := tx.Exec(ctx, `SELECT set_config('app.agent','on',true)`); err != nil {
						return err
					}
					before := readSettings(tx)
					if before[2] != "on" {
						t.Fatal("caller has no restrictive site scope")
					}
					n, err := sqlc.New(tx).ClearAbilityRequestContent(ctx, limit)
					if err != nil {
						return err
					}
					if n != limit {
						t.Fatalf("cross-tenant batch: cleared=%d limit=%d", n, limit)
					}
					if after := readSettings(tx); before != after {
						t.Fatalf("retention changed caller settings: %v -> %v", before, after)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			assertCleared := func(tenantID, siteID, id uuid.UUID, want bool) {
				t.Helper()
				if err := app.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
					row, err := sqlc.New(tx).GetAbilityRequestForSite(ctx, sqlc.GetAbilityRequestForSiteParams{TenantID: tenantID, SiteID: siteID, ID: id})
					if err != nil {
						return err
					}
					if row.ContentClearedAt.Valid != want {
						t.Fatalf("request %s cleared=%t want=%t", id, row.ContentClearedAt.Valid, want)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}

			// Locked candidates are skipped without using up the clearing limit.
			if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
				var id uuid.UUID
				if err := tx.QueryRow(ctx, `SELECT id FROM assistant_ability_requests WHERE id=$1::uuid FOR UPDATE`, locked).Scan(&id); err != nil {
					return err
				}
				clearScoped(2)
				assertCleared(tenant, site, locked, false)
				assertCleared(tenant, site, first, true)
				assertCleared(otherTenant, otherSite, second, true)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := sqlc.New(app.Pool).ClearAbilityRequestContent(ctx, 1); err != nil || n != 1 {
				t.Fatalf("unlocked candidate: cleared=%d err=%v", n, err)
			}
			first = r01Request(t, app, tenant, site, "done", 16*24*time.Hour, false, "wpmgr/page-create", 603)
			second = r01Request(t, app, otherTenant, otherSite, "done", 16*24*time.Hour, false, "wpmgr/page-create", 604)
			clearScoped(1)
			var cleared int
			for _, target := range []struct{ tenant, site, id uuid.UUID }{{tenant, site, first}, {otherTenant, otherSite, second}} {
				if err := app.InTenantTx(ctx, target.tenant, func(tx pgx.Tx) error {
					row, err := sqlc.New(tx).GetAbilityRequestForSite(ctx, sqlc.GetAbilityRequestForSiteParams{TenantID: target.tenant, SiteID: target.site, ID: target.id})
					if err == nil && row.ContentClearedAt.Valid {
						cleared++
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if cleared != 1 {
				t.Fatalf("batch limit applied across tenants: cleared=%d", cleared)
			}

			// Skipped row failures and invalid calls leave the caller's scope intact.
			if _, err := owner.Exec(ctx, `CREATE FUNCTION public.r01_retention_scope_failure() RETURNS trigger
				LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'retention scope test failure' USING ERRCODE='P0001'; END $$;
				CREATE TRIGGER r01_retention_scope_failure BEFORE UPDATE OF content_cleared_at
				ON public.assistant_ability_requests FOR EACH ROW EXECUTE FUNCTION public.r01_retention_scope_failure()`); err != nil {
				t.Fatal(err)
			}
			if err := app.RunTenantTx(ctx, acprSitePrincipal(tenant, site), func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.agent','on',true)`); err != nil {
					return err
				}
				before := readSettings(tx)
				n, err := sqlc.New(tx).ClearAbilityRequestContent(ctx, 2)
				if err != nil {
					return err
				}
				if n != 0 {
					t.Fatalf("failing rows were counted as cleared: %d", n)
				}
				if after := readSettings(tx); before != after {
					t.Fatalf("skipped retention changed caller settings: %v -> %v", before, after)
				}
				if _, err := tx.Exec(ctx, `DO $$ BEGIN
                    BEGIN
                        PERFORM public.clear_ability_request_content(0);
                        RAISE EXCEPTION 'invalid retention limit was accepted' USING ERRCODE='XX000';
                    EXCEPTION WHEN SQLSTATE '22023' THEN NULL;
                    END;
                END $$`); err != nil {
					return err
				}
				if after := readSettings(tx); before != after {
					t.Fatalf("failed retention changed caller settings: %v -> %v", before, after)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestR01RetentionUnusableJSON(t *testing.T) {
	for _, single := range []bool{true, false} {
		t.Run(fmt.Sprintf("single_role_%t", single), func(t *testing.T) {
			app, _ := ds01Postgres(t, single, false)
			ctx := context.Background()
			type target struct {
				tenant, site, id uuid.UUID
				nullDesign       bool
			}
			var targets []target
			for _, name := range []string{"A", "B", "C"} {
				tenant, site := ds01Seed(t, app)
				targets = append(targets, target{tenant: tenant, site: site, id: r01Request(t, app, tenant, site, "expired", 19*24*time.Hour, false, "wpmgr/page-create", 700)})
				if name == "B" {
					input := `{"post_type":"page","title":"A\u0000B","outline":[{"type":"paragraph","text":"Hi"}]}`
					if !json.Valid([]byte(input)) {
						t.Fatal("unusable jsonb fixture is not valid JSON")
					}
					targets = append(targets, target{tenant: tenant, site: site, id: r01RequestWithInput(t, app, tenant, site, "expired", 18*24*time.Hour, false, "wpmgr/page-create", 701, input), nullDesign: true})
					targets = append(targets, target{tenant: tenant, site: site, id: r01Request(t, app, tenant, site, "expired", 17*24*time.Hour, false, "wpmgr/page-create", 702)})
				}
			}
			n, err := sqlc.New(app.Pool).ClearAbilityRequestContent(ctx, int32(len(targets)))
			if err != nil || n != int32(len(targets)) {
				t.Fatalf("unusable JSON batch: cleared=%d want=%d err=%v", n, len(targets), err)
			}
			for _, target := range targets {
				if err := app.InTenantTx(ctx, target.tenant, func(tx pgx.Tx) error {
					mcpAssertAndReportRole(t, tx, "R-01 unusable JSON record")
					row, err := sqlc.New(tx).GetAbilityRequestForSite(ctx, sqlc.GetAbilityRequestForSiteParams{TenantID: target.tenant, SiteID: target.site, ID: target.id})
					if err != nil {
						return err
					}
					if !row.ContentClearedAt.Valid || row.InputJson != "{}" || len(row.CardFacts) != 0 {
						t.Fatalf("request %s was not cleared", target.id)
					}
					if target.nullDesign {
						var draft map[string]any
						if err := json.Unmarshal(row.RetainedDraft, &draft); err != nil {
							return err
						}
						version, hasVersion := draft["outline_version"]
						design, hasDesign := draft["design"]
						if len(draft) != 2 || !hasVersion || !hasDesign || version != nil || design != nil {
							t.Fatalf("unusable JSON design=%s", row.RetainedDraft)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestR01RetentionSkipsFailedRow(t *testing.T) {
	for _, single := range []bool{true, false} {
		t.Run(fmt.Sprintf("single_role_%t", single), func(t *testing.T) {
			app, owner := ds01Postgres(t, single, false)
			ctx := context.Background()
			tenant, site := ds01Seed(t, app)
			failed := r01Request(t, app, tenant, site, "expired", 18*24*time.Hour, false, "wpmgr/page-create", 710)
			before := r01Request(t, app, tenant, site, "expired", 19*24*time.Hour, false, "wpmgr/page-create", 711)
			after := r01Request(t, app, tenant, site, "expired", 17*24*time.Hour, false, "wpmgr/page-create", 712)
			otherTenant, otherSite := ds01Seed(t, app)
			other := r01Request(t, app, otherTenant, otherSite, "expired", 17*24*time.Hour, false, "wpmgr/page-create", 713)
			if _, err := owner.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION public.r01_retention_one_failure() RETURNS trigger
				LANGUAGE plpgsql AS $$ BEGIN
					IF NEW.id='%s'::uuid THEN RAISE EXCEPTION 'retention row test failure' USING ERRCODE='P0001'; END IF;
					RETURN NEW;
				END $$;
				CREATE TRIGGER r01_retention_one_failure BEFORE UPDATE OF content_cleared_at ON public.assistant_ability_requests
				FOR EACH ROW EXECUTE FUNCTION public.r01_retention_one_failure()`, failed)); err != nil {
				t.Fatal(err)
			}
			good := []struct{ tenant, site, id uuid.UUID }{{tenant, site, before}, {tenant, site, after}, {otherTenant, otherSite, other}}
			n, err := sqlc.New(app.Pool).ClearAbilityRequestContent(ctx, int32(len(good)+1))
			if err != nil || n != int32(len(good)) {
				t.Fatalf("isolated row failure: cleared=%d want=%d err=%v", n, len(good), err)
			}
			for _, target := range append(good, struct{ tenant, site, id uuid.UUID }{tenant, site, failed}) {
				if err := app.InTenantTx(ctx, target.tenant, func(tx pgx.Tx) error {
					row, err := sqlc.New(tx).GetAbilityRequestForSite(ctx, sqlc.GetAbilityRequestForSiteParams{TenantID: target.tenant, SiteID: target.site, ID: target.id})
					if err != nil {
						return err
					}
					if row.ContentClearedAt.Valid != (target.id != failed) {
						t.Fatalf("request %s cleared=%t", target.id, row.ContentClearedAt.Valid)
					}
					if target.id == failed && row.InputJson != r01Input {
						t.Fatal("failed row partially cleared")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
