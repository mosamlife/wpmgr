package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// TestDS02DeleteSiteTiming measures the migrated schema using the application
// role and production tenant transaction helper. It has no latency threshold.
// WPMGR_DS02_ROWS_<UPPERCASE_TABLE> overrides rows per site; all sites receive
// identical counts, scaled equally when a pilot projects beyond the size cap.
// WPMGR_DS02_OTHER_ROWS supplies the default for large tables
// other than RUM, activity, and the backup file index. Counts must be positive.
func TestDS02DeleteSiteTiming(t *testing.T) {
	if os.Getenv("WPMGR_DS02_TIMING") != "1" {
		t.Skip("set WPMGR_DS02_TIMING=1 to seed history and measure DeleteSite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 29*time.Minute)
	defer cancel()
	seeds, err := ds02Seeds()
	if err != nil {
		t.Fatal(err)
	}
	// This harness applies the embedded migrations without adding table grants.
	app, owner, reset := ds02Postgres(t, ctx)
	var version string
	if err := app.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("Postgres=%s; dataset cap=%d bytes (database heap, TOAST, indexes and catalogs; WAL excluded)", version, ds02MaxBytes)
	baseline, err := ds02Size(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range seeds {
		t.Logf("rows per site %s=%d", seed.table, seed.rows)
	}
	newTenants := func() []uuid.UUID {
		return []uuid.UUID{seedTenant(t, app, "ds02-a-"+uuid.NewString()), seedTenant(t, app, "ds02-b-"+uuid.NewString()), seedTenant(t, app, "ds02-c-"+uuid.NewString())}
	}
	tenants := newTenants()
	calibrated := false
	var target uuid.UUID
	for i := 0; i <= 20; i++ {
		tenant := tenants[i%len(tenants)]
		siteID := ds02SeedSite(t, ctx, app, tenant)
		if i == 0 {
			target = siteID
		}
		for _, seed := range seeds {
			if err := ds02SeedTable(ctx, app, tenant, siteID, seed); err != nil {
				t.Fatalf("seed site %d table %s: %v", i, seed.table, err)
			}
			if _, err := ds02Size(ctx, app); err != nil {
				t.Fatal(err)
			}
		}
		size, err := ds02Size(ctx, app)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("seeded site=%d tenant=%s database_bytes=%d", i, tenant, size)
		if i == 0 && !calibrated {
			calibrated = true
			projected := baseline + (size-baseline)*21
			if projected > ds02MaxBytes*7/10 {
				// TRUNCATE only resets fixtures in this throwaway database. The
				// measured deletes below always use the application role.
				scale := float64(ds02MaxBytes*6/10-baseline) / float64((size-baseline)*21)
				t.Logf("pilot database_bytes=%d projected_bytes=%d; uniformly scaling requested row counts by %.6f", size, projected, scale)
				if err := reset(ctx); err != nil {
					t.Fatal(err)
				}
				seeds, err = ds02SeedsAtScale(scale)
				if err != nil {
					t.Fatal(err)
				}
				for _, seed := range seeds {
					t.Logf("effective rows per site %s=%d", seed.table, seed.rows)
				}
				tenants = newTenants()
				i = -1
				continue
			}
		}
	}
	// Planner statistics are collected after seeding; no index or FK is changed.
	for _, seed := range seeds {
		if _, err := owner.Exec(ctx, "ANALYZE "+pgx.Identifier{seed.table}.Sanitize()); err != nil {
			t.Fatalf("analyze %s: %v", seed.table, err)
		}
	}
	statement, err := ds02DeleteStatement()
	if err != nil {
		t.Fatal(err)
	}
	rollbackMeasurement := errors.New("DS-02 roll back EXPLAIN")
	var explained []byte
	err = app.InTenantTx(ctx, tenants[0], func(tx pgx.Tx) error {
		var role string
		var superuser, bypassRLS bool
		if err := tx.QueryRow(ctx, `SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&role, &superuser, &bypassRLS); err != nil {
			return err
		}
		t.Logf("measurement role=%s superuser=%t bypassrls=%t", role, superuser, bypassRLS)
		if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement, target, tenants[0]).Scan(&explained); err != nil {
			return err
		}
		return rollbackMeasurement
	})
	if !errors.Is(err, rollbackMeasurement) {
		t.Fatalf("EXPLAIN rolled-back delete: %v", err)
	}
	report, err := ds02ExplainReport(explained)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("EXPLAIN JSON (includes BUFFERS): %s", explained)
	t.Logf("EXPLAIN timings (descending; trigger timings are inclusive):\n%s", report)
	if _, err := ds02Size(ctx, app); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var deletedRows int64
	err = app.InTenantTx(ctx, tenants[0], func(tx pgx.Tx) error {
		var err error
		deletedRows, err = sqlc.New(tx).DeleteSite(ctx, sqlc.DeleteSiteParams{ID: target, TenantID: tenants[0]})
		return err
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("committed DeleteSite: %v", err)
	}
	t.Logf("DeleteSite committed wall_time=%s (tenant transaction, statement and commit; cache warmed by EXPLAIN)", elapsed)
	t.Logf("DeleteSite affected_rows=%d", deletedRows)
	if _, err := ds02Size(ctx, app); err != nil {
		t.Fatal(err)
	}
}

const ds02MaxBytes int64 = 2_000_000_000

func ds02Size(ctx context.Context, app *db.Pool) (int64, error) {
	var size int64
	if err := app.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&size); err != nil {
		return 0, err
	}
	// Leave space for a bulk insert and the subsequent rolled-back measurement.
	if size >= ds02MaxBytes*9/10 {
		return size, fmt.Errorf("dataset near cap: bytes=%d cap=%d; lower WPMGR_DS02_ROWS_<TABLE> or WPMGR_DS02_OTHER_ROWS", size, ds02MaxBytes)
	}
	return size, nil
}

func ds02DeleteStatement() (string, error) {
	data, err := os.ReadFile("../db/query/sites.sql")
	if err != nil {
		return "", err
	}
	match := regexp.MustCompile(`(?s)-- name: DeleteSite :execrows\s*(DELETE\b[^;]+;)`).FindSubmatch(data)
	if len(match) != 2 {
		return "", errors.New("production DeleteSite statement not found")
	}
	return string(match[1]), nil
}

func ds02ExplainReport(data []byte) (string, error) {
	type trigger struct {
		Constraint string   `json:"Constraint Name"`
		Name       string   `json:"Trigger Name"`
		Relation   string   `json:"Relation"`
		Time       *float64 `json:"Time"`
		Calls      *int64   `json:"Calls"`
	}
	var plans []struct {
		Triggers []trigger `json:"Triggers"`
		Total    *float64  `json:"Execution Time"`
	}
	if err := json.Unmarshal(data, &plans); err != nil {
		return "", err
	}
	if len(plans) != 1 || plans[0].Total == nil || len(plans[0].Triggers) == 0 {
		return "", errors.New("EXPLAIN did not report execution and trigger timings")
	}
	for _, tr := range plans[0].Triggers {
		if tr.Time == nil || tr.Calls == nil || *tr.Time < 0 || *tr.Calls <= 0 || tr.Relation == "" || (tr.Constraint == "" && tr.Name == "") {
			return "", errors.New("EXPLAIN trigger has incomplete timing data")
		}
	}
	sort.SliceStable(plans[0].Triggers, func(i, j int) bool { return *plans[0].Triggers[i].Time > *plans[0].Triggers[j].Time })
	var out strings.Builder
	var total float64
	for _, tr := range plans[0].Triggers {
		label := "Trigger " + tr.Name
		if tr.Constraint != "" {
			label = "Trigger for constraint " + tr.Constraint
		}
		fmt.Fprintf(&out, "%s on %s: time=%.3f ms calls=%d\n", label, tr.Relation, *tr.Time, *tr.Calls)
		total += *tr.Time
	}
	fmt.Fprintf(&out, "trigger_total=%.3f ms execution_total=%.3f ms", total, *plans[0].Total)
	return out.String(), nil
}

type ds02Seed struct {
	table  string
	rows   int64
	values map[string]string
	clone  bool
}

func ds02EnvRows(name string, fallback int64) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 1 || n > 1_000_000 {
		return 0, fmt.Errorf("%s must be between 1 and 1000000, got %q", name, value)
	}
	return n, nil
}

func ds02Seeds() ([]ds02Seed, error) {
	return ds02SeedsAtScale(1)
}

func ds02SeedsAtScale(scale float64) ([]ds02Seed, error) {
	if scale <= 0 || scale > 1 {
		return nil, fmt.Errorf("invalid dataset scale %v", scale)
	}
	other, err := ds02EnvRows("WPMGR_DS02_OTHER_ROWS", 10_000)
	if err != nil {
		return nil, err
	}
	// Parent histories are bounded; leaf/event tables get the bulk history.
	seeds := []ds02Seed{
		{"site_destinations", 1, map[string]string{"id": "", "kind": "'cp'", "label": "'DS-02'"}, false},
		{"backup_schedules", 1, map[string]string{"id": "", "enabled": "false"}, false},
		{"backup_snapshots", 120, map[string]string{"id": "", "kind": "'full'", "status": "'completed'", "destination_id": "@site_destinations", "parent_snapshot_id": "@backup_snapshots:first", "base_snapshot_id": "@backup_snapshots:first"}, false},
		{"update_runs", 120, map[string]string{"id": "", "status": "'completed'"}, false},
		{"restore_runs", 120, map[string]string{"id": "", "snapshot_id": "@backup_snapshots", "status": "'completed'"}, false},
		{"clients", 1, map[string]string{"id": "", "name": "'DS-02'"}, false},
		{"report_schedules", 1, map[string]string{"id": "", "client_id": "@clients", "enabled": "false"}, false},
		{"scan_runs", other, map[string]string{"id": "", "status": "'done'"}, false},
		{"site_media_assets", other, map[string]string{"id": "", "wp_attachment_id": "g", "title": "'asset'", "original_path": "'img/'||g", "original_url": "'https://example.test/'||g", "original_mime": "'image/jpeg'", "original_size_bytes": "1024", "current_size_bytes": "1024"}, false},
		{"rucss_results", other, map[string]string{"id": "", "structure_hash": "md5(g::text)", "used_css_s3_key": "'css/'||g"}, false},
		{"media_optimization_jobs", other, map[string]string{"id": "p.site::text||':'||g", "asset_id": "@site_media_assets", "wp_attachment_id": "g", "kind": "'optimize'", "state": "'completed'"}, false},
		{"rum_events_raw", 200_000, map[string]string{"metric": "'lcp'", "url_pattern": "'/page/'||(g%100)", "value_milli": "1500", "received_at": "@time"}, false},
		{"agent_activity_log", 50_000, map[string]string{"seq": "g", "event_type": "'post.updated'", "object_type": "'post'", "prev_hash": "repeat('0',64)", "this_hash": "repeat('1',64)", "occurred_at": "@time"}, false},
		{"backup_file_index", 200_000, map[string]string{"snapshot_id": "@backup_snapshots", "file_path": "'wp-content/f/'||g", "file_size": "1024"}, false},
		{"backup_manifest_entries", other, map[string]string{"snapshot_id": "@backup_snapshots", "path": "'wp-content/f/'||g", "size": "1024"}, false},
		{"backup_chunks", other, map[string]string{"blake3": "md5(p.site::text||g::text)||md5(g::text)", "s3_key": "p.site::text||'/chunk/'||g", "size": "1024"}, false},
		{"audit_log", other, map[string]string{"actor_type": "'system'", "action": "'measurement'", "hash": "repeat('1',64)"}, false},
		{"billing_events", other, map[string]string{"provider": "'measurement'", "provider_event_id": "p.site::text||':'||g", "kind": "'measurement'", "occurred_at": "@time"}, false},
		{"update_tasks", other, map[string]string{"run_id": "@update_runs", "target_type": "'core'", "target_slug": "'core'", "status": "'completed'"}, false},
		{"backup_schedule_runs", other, map[string]string{"schedule_id": "@backup_schedules", "snapshot_id": "@backup_snapshots", "scheduled_for": "@time", "status": "'completed'"}, false},
		{"restore_run_events", other, map[string]string{"restore_run_id": "@restore_runs", "phase": "'complete'", "occurred_at": "@time"}, false},
		{"site_uptime_probes", other, map[string]string{"up": "true", "http_status": "200", "probed_at": "@time"}, false},
		{"agent_php_errors", other, map[string]string{"md5": "md5(g::text)", "code": "2", "message": "'measurement warning'", "first_seen_at": "@time", "last_seen_at": "@time"}, false},
		{"agent_login_events", other, map[string]string{"agent_event_id": "g", "status": "2", "occurred_at": "@time"}, false},
		{"site_connection_history", other, map[string]string{"from_state": "'connected'", "to_state": "'degraded'", "occurred_at": "@time"}, false},
		{"site_events", other, map[string]string{"event_id": "p.site::text||':'||g", "type": "'measurement'"}, false},
		{"media_variant_results", other, map[string]string{"job_id": "p.site::text||':'||(1+(g-1)%@count:media_optimization_jobs)", "variant_name": "'thumbnail'", "source_size_bytes": "1024", "source_mime": "'image/jpeg'", "state": "'completed'"}, false},
		{"email_log", other, map[string]string{"to_addresses": "ARRAY['recipient@example.test']", "subject": "'measurement'", "template": "'measurement'", "status": "'sent'"}, false},
		{"site_cache_hit_ratio_history", other, map[string]string{"sampled_at": "@time"}, false},
		{"cache_purge_audit", other, map[string]string{"kind": "'all'"}, false},
		{"rucss_jobs", other, map[string]string{"id": "p.site::text||':'||g", "result_id": "@rucss_results", "state": "'completed'"}, false},
		{"site_db_size_history", other, map[string]string{"scanned_at": "@time", "db_size_bytes": "1048576"}, false},
		{"font_transcode_results", other, map[string]string{"source_hash": "md5(p.site::text||g::text)"}, false},
		{"font_results", other, map[string]string{"source_hash": "md5(g::text)", "state": "'ready'"}, false},
		{"rum_rollup_hourly", other, map[string]string{"url_pattern": "'/page/'||g", "metric": "'lcp'", "device": "'desktop'", "country": "'US'", "bucket_hour": "date_trunc('hour',@time)"}, false},
		{"rum_rollup_daily", other, map[string]string{"url_pattern": "'/page/'||g", "metric": "'lcp'", "device": "'desktop'", "country": "'US'", "bucket_day": "(@time)::date"}, false},
		{"site_email_log", other, map[string]string{"agent_seq": "g", "status": "'sent'"}, false},
		{"email_suppression", other, map[string]string{"email_hash": "decode(md5(g::text)||md5(g::text),'hex')"}, false},
		{"generated_reports", other, map[string]string{"client_id": "@clients", "schedule_id": "@report_schedules", "period_start": "@time", "period_end": "@time + interval '1 day'", "status": "'completed'"}, false},
		{"site_object_cache_stats_history", other, map[string]string{"sampled_at": "@time"}, false},
		{"site_security_bans", other, map[string]string{"type": "'user_agent'", "value": "'measurement-'||g"}, false},
		{"scan_run_hashes", other, map[string]string{"run_id": "@scan_runs", "path": "'file/'||g"}, false},
		{"scan_findings", other, map[string]string{"run_id": "@scan_runs", "last_seen_run": "@scan_runs", "finding_type": "'core_modified'", "path": "'file/'||g", "severity": "'low'", "dedup_key": "md5(g::text)"}, false},
		{"site_file_baseline", other, map[string]string{"path": "'file/'||g", "md5": "md5(g::text)", "updated_run": "@scan_runs"}, false},
		{"site_managed_files", other, map[string]string{"path": "'file/'||g"}, false},
		{"site_vulnerabilities", other, map[string]string{"vuln_id": "'v/'||g", "kind": "'core'", "slug": "'core'", "name": "'Core'", "installed_version": "'6.0'", "severity": "'low'", "title": "'measurement'", "status": "'resolved'"}, false},
		{"file_transfers", other, map[string]string{"direction": "'download'", "rel_path": "'file/'||g", "expires_at": "@time + interval '1 day'"}, false},
		{"site_content_inventory", other, map[string]string{"post_id": "g", "post_type": "'page'", "post_status": "'publish'", "verdict": "'classic'", "route_number": "1", "route_reason": "'content_column'", "checked_at": "@time"}, false},
		{"site_ability_inventory", other, map[string]string{"name": "'core/measurement-'||g", "owner_kind": "'core'", "checked_at": "@time"}, false},
		{"site_context_versions", other, map[string]string{"version": "g", "author_type": "'system'", "provenance": "'manual'"}, false},
		{"org_context_versions", other, map[string]string{"version": "g + @siteordinal * @count:org_context_versions", "author_type": "'system'", "provenance": "'manual'"}, false},
		{"assistant_update_proposals", other, map[string]string{"id": "gen_random_uuid()", "state": "'expired'", "created_at": "@time", "expires_at": "@time + interval '1 hour'"}, true},
		{"assistant_cache_purge_requests", other, map[string]string{"id": "gen_random_uuid()", "state": "'pending'", "proposed_by_grant_id": "gen_random_uuid()", "created_at": "@time", "expires_at": "@time + interval '1 hour'"}, true},
		{"assistant_ability_requests", other, map[string]string{"id": "gen_random_uuid()", "state": "'pending'", "proposed_by_grant_id": "gen_random_uuid()", "created_at": "@time", "expires_at": "@time + interval '1 hour'"}, true},
	}
	counts := make(map[string]int64)
	for i := range seeds {
		seeds[i].rows, err = ds02EnvRows("WPMGR_DS02_ROWS_"+strings.ToUpper(seeds[i].table), seeds[i].rows)
		if err != nil {
			return nil, err
		}
		seeds[i].rows = max(1, int64(float64(seeds[i].rows)*scale))
		counts[seeds[i].table] = seeds[i].rows
	}
	if counts["backup_schedules"] > 1 || counts["report_schedules"] > counts["clients"] {
		return nil, errors.New("seed counts require one backup schedule per site and at most one report schedule per client")
	}
	for i := range seeds {
		for column, value := range seeds[i].values {
			if value == "" {
				value = ds02ID(seeds[i].table, "g")
			} else if regexp.MustCompile(`^@[a-z_]+(?::first)?$`).MatchString(value) && value != "@time" {
				parent := strings.TrimPrefix(value, "@")
				count, found := counts[strings.TrimSuffix(parent, ":first")]
				if !found {
					return nil, fmt.Errorf("unknown seed parent %q", parent)
				}
				ordinal := "1+(g-1)%" + strconv.FormatInt(count, 10)
				if strings.HasSuffix(parent, ":first") {
					parent = strings.TrimSuffix(parent, ":first")
					ordinal = "1"
					value = "CASE WHEN g=1 THEN NULL ELSE " + ds02ID(parent, ordinal) + " END"
				} else {
					value = ds02ID(parent, ordinal)
				}
			}
			for table, count := range counts {
				value = strings.ReplaceAll(value, "@count:"+table, strconv.FormatInt(count, 10))
			}
			seeds[i].values[column] = value
		}
	}
	return seeds, nil
}

// ds02Postgres follows startPostgres's owner/app role provisioning and embedded
// migrator, without the test harness's supplemental application grants.
func ds02Postgres(t *testing.T, ctx context.Context) (app, owner *db.Pool, reset func(context.Context) error) {
	t.Helper()
	skipIfDockerUnavailable(t, ctx, "DS-02 postgres")
	c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("wpmgr"), tcpostgres.WithUsername("wpmgr"), tcpostgres.WithPassword("wpmgr"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(time.Minute)))
	if c != nil {
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// Terminate removes the container and its anonymous data volumes.
			if err := c.Terminate(cleanupCtx); err != nil {
				t.Errorf("DS-02 container/volume cleanup: %v", err)
			}
		})
	}
	if err != nil {
		setupFatalfOrSkipIfDaemonDied(t, ctx, err, "DS-02 container start")
	}
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bootstrap.Close)
	for _, stmt := range []string{
		"CREATE ROLE wpmgr_owner LOGIN PASSWORD 'owner' NOSUPERUSER NOBYPASSRLS CREATEROLE",
		"ALTER DATABASE wpmgr OWNER TO wpmgr_owner",
	} {
		if _, err := bootstrap.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	owner, err = db.Connect(ctx, strings.Replace(dsn, "wpmgr:wpmgr@", "wpmgr_owner:owner@", 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'`); err != nil {
		t.Fatal(err)
	}
	app, err = db.Connect(ctx, strings.Replace(dsn, "wpmgr:wpmgr@", "wpmgr_app:app@", 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	reset = func(ctx context.Context) error {
		_, err := bootstrap.Exec(ctx, `TRUNCATE tenants CASCADE`)
		return err
	}
	return app, owner, reset
}

func ds02ID(table, ordinal string) string {
	return "md5(p.site::text||':" + table + ":'||(" + ordinal + ")::text)::uuid"
}

func ds02SeedSite(t *testing.T, ctx context.Context, app *db.Pool, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	var siteID uuid.UUID
	if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO sites (tenant_id,url,name) VALUES ($1,$2,'DS-02') RETURNING id`, tenant, "https://"+uuid.NewString()+".example.test").Scan(&siteID)
	}); err != nil {
		t.Fatal(err)
	}
	m133InsertPending(t, app, tenant, siteID, uuid.New(), "ds02")
	acprInsert(t, app, acprOrgPrincipal(tenant), acprParams(tenant, siteID, uuid.New(), "ds02"))
	aarInsert(t, app, acprOrgPrincipal(tenant), aarParams(tenant, siteID, uuid.New(), "ds02"))
	return siteID
}

func ds02SeedTable(ctx context.Context, app *db.Pool, tenant, siteID uuid.UUID, seed ds02Seed) error {
	// Catalog columns let defaults and generated columns remain schema-owned.
	rows, err := app.Query(ctx, `SELECT attname FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped AND attgenerated='' ORDER BY attnum`, "public."+seed.table)
	if err != nil {
		return err
	}
	var available []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			rows.Close()
			return err
		}
		available = append(available, column)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(available) == 0 {
		return fmt.Errorf("no columns in %s", seed.table)
	}
	values := make(map[string]string)
	for column, value := range seed.values {
		values[column] = value
	}
	var columns, expressions []string
	for _, column := range available {
		if column == "tenant_id" {
			values[column] = "p.tenant"
		} else if column == "site_id" {
			values[column] = "p.site"
		} else if column == "created_at" && !seed.clone {
			values[column] = "@time"
		}
		value, supplied := values[column]
		if !supplied && !seed.clone {
			continue
		}
		if !supplied {
			value = "template." + pgx.Identifier{column}.Sanitize()
		}
		value = strings.ReplaceAll(value, "@time", "(now()-interval '120 days'+interval '120 days'*(g::double precision/p.total))")
		value = strings.ReplaceAll(value, "@siteordinal", "(SELECT count(*) FROM sites WHERE tenant_id=p.tenant)")
		columns = append(columns, pgx.Identifier{column}.Sanitize())
		expressions = append(expressions, value)
		delete(values, column)
	}
	if len(values) != 0 {
		return fmt.Errorf("unknown seed columns for %s: %v", seed.table, values)
	}
	query := "INSERT INTO " + pgx.Identifier{seed.table}.Sanitize() + " (" + strings.Join(columns, ",") + ") SELECT " + strings.Join(expressions, ",") +
		" FROM generate_series($3::bigint,$4::bigint) AS g CROSS JOIN (SELECT $1::uuid AS tenant,$2::uuid AS site,$5::bigint AS total) p"
	first := int64(1)
	if seed.clone {
		query += " CROSS JOIN (SELECT * FROM " + pgx.Identifier{seed.table}.Sanitize() + " WHERE tenant_id=$1 AND site_id=$2 ORDER BY created_at,id LIMIT 1) template"
		first = 2
	}
	for from := first; from <= seed.rows; from += 2000 {
		to := min(from+1999, seed.rows)
		if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, query, tenant, siteID, from, to, seed.rows)
			if err == nil && tag.RowsAffected() != to-from+1 {
				return fmt.Errorf("inserted %d rows, want %d", tag.RowsAffected(), to-from+1)
			}
			return err
		}); err != nil {
			return err
		}
		if _, err := ds02Size(ctx, app); err != nil {
			return err
		}
	}
	if seed.clone {
		// Approval requests enter pending; expiry is a permitted state update.
		return app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "UPDATE "+pgx.Identifier{seed.table}.Sanitize()+" SET state='expired' WHERE tenant_id=$1 AND site_id=$2 AND state='pending'", tenant, siteID)
			return err
		})
	}
	return nil
}
