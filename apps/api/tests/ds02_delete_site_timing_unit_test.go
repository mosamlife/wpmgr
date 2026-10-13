package tests

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestDS02ExplainReport(t *testing.T) {
	input := []byte(`[{"Plan":{"Node Type":"ModifyTable"},"Triggers":[
		{"Constraint Name":"fast_fk","Relation":"fast","Time":1.25,"Calls":2},
		{"Constraint Name":"slow_fk","Relation":"slow","Time":12.5,"Calls":3},
		{"Trigger Name":"lifecycle","Relation":"sites","Time":0.5,"Calls":1}
	],"Execution Time":15.75}]`)
	report, err := ds02ExplainReport(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "Trigger for constraint slow_fk on slow: time=12.500 ms calls=3") ||
		!strings.Contains(report, "trigger_total=14.250 ms execution_total=15.750 ms") ||
		strings.Index(report, "slow_fk") > strings.Index(report, "fast_fk") ||
		!strings.Contains(report, "Trigger lifecycle on sites") {
		t.Fatalf("incomplete or unsorted timing report:\n%s", report)
	}
	for _, bad := range []string{`[]`, `[{"Execution Time":1}]`, `[{"Triggers":[],"Execution Time":1}]`,
		`[{"Triggers":[{"Constraint Name":"fk","Time":1,"Calls":1}]}]`, `garbage`} {
		if _, err := ds02ExplainReport([]byte(bad)); err == nil {
			t.Errorf("accepted missing timing data: %s", bad)
		}
	}
}

func TestDS02SeedPlan(t *testing.T) {
	t.Setenv("WPMGR_DS02_OTHER_ROWS", "10000")
	seeds, err := ds02Seeds()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, seed := range seeds {
		if seed.rows <= 0 || seen[seed.table] {
			t.Fatalf("invalid seed: %s rows=%d", seed.table, seed.rows)
		}
		for _, value := range seed.values {
			remaining := strings.ReplaceAll(strings.ReplaceAll(value, "@time", ""), "@siteordinal", "")
			remaining = regexp.MustCompile(`'(?:''|[^'])*'`).ReplaceAllString(remaining, "")
			if strings.Contains(remaining, "@") || strings.Contains(value, "%0") {
				t.Fatalf("unresolved seed expression on %s: %s", seed.table, value)
			}
			for _, match := range regexp.MustCompile(`':(\w+):'`).FindAllStringSubmatch(value, -1) {
				if match[1] != seed.table && !seen[match[1]] {
					t.Fatalf("seed %s references parent %s before it is inserted", seed.table, match[1])
				}
			}
		}
		seen[seed.table] = true
	}
	t.Setenv("WPMGR_DS02_ROWS_BACKUP_SNAPSHOTS", "7")
	t.Setenv("WPMGR_DS02_ROWS_RUM_EVENTS_RAW", "11")
	seeds, err = ds02Seeds()
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range seeds {
		if seed.table == "backup_file_index" && !strings.Contains(seed.values["snapshot_id"], "%7") {
			t.Fatalf("dependent volume override not respected: %s", seed.values["snapshot_id"])
		}
		if seed.table == "rum_events_raw" && seed.rows != 11 {
			t.Fatalf("RUM override rows=%d", seed.rows)
		}
	}
	for _, value := range []string{"0", "-1", "garbage", "1000001"} {
		t.Setenv("WPMGR_DS02_ROWS_RUM_EVENTS_RAW", value)
		if _, err := ds02Seeds(); err == nil {
			t.Errorf("accepted invalid row count %q", value)
		}
	}

	// The static audit and measurement use the same bulk-table classification.
	data, err := os.ReadFile("ds02_fk_index_audit.py")
	if err != nil {
		t.Fatal(err)
	}
	classification := regexp.MustCompile(`(?s)LARGE = set\("""(.*?)"""\.split\(\)\)`).FindSubmatch(data)
	if len(classification) != 2 {
		t.Fatal("static audit LARGE classification missing")
	}
	large := make(map[string]bool)
	for _, table := range strings.Fields(string(classification[1])) {
		large[table] = true
	}
	for _, seed := range seeds {
		if seed.rows == 10_000 || seed.table == "rum_events_raw" || seed.table == "agent_activity_log" || seed.table == "backup_file_index" {
			if !large[seed.table] {
				t.Errorf("bulk table %s missing from audit", seed.table)
			}
			delete(large, seed.table)
		}
	}
	if len(large) != 0 {
		t.Fatalf("large audit tables without fixtures: %v", large)
	}
}

func TestDS02DatasetScaling(t *testing.T) {
	t.Setenv("WPMGR_DS02_OTHER_ROWS", "10000")
	t.Setenv("WPMGR_DS02_ROWS_RUM_EVENTS_RAW", "200000")
	t.Setenv("WPMGR_DS02_ROWS_BACKUP_SNAPSHOTS", "120")
	full, err := ds02Seeds()
	if err != nil {
		t.Fatal(err)
	}
	for _, scale := range []float64{0.5, 0.000001} {
		scaled, err := ds02SeedsAtScale(scale)
		if err != nil {
			t.Fatal(err)
		}
		for i, seed := range scaled {
			if seed.rows != max(1, int64(float64(full[i].rows)*scale)) {
				t.Fatalf("nonuniform scaling for %s: rows=%d", seed.table, seed.rows)
			}
			for _, value := range seed.values {
				if strings.Contains(value, "%0") {
					t.Fatalf("zero parent count after scaling: %s %s", seed.table, value)
				}
			}
		}
	}
	for _, scale := range []float64{0, -1, 2} {
		if _, err := ds02SeedsAtScale(scale); err == nil {
			t.Errorf("accepted invalid scale %v", scale)
		}
	}
}
