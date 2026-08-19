package update

// inflight_status_guard_test.go binds InFlightTaskStatuses to the two SQL
// guards that are its only real consumers.
//
// Why this is a text test. The set exists three times: once here in Go, once as
// the predicate of the update_tasks_inflight_target_idx partial unique index in
// db/schema.sql, and once as the reaper's WHERE clause in
// db/query/updates.sql. The two SQL copies cannot import the Go one — one is a
// DDL predicate the database compiles, the other is a string sqlc reads at
// generate time — and neither is reachable from a Go value at runtime. sqlc
// generates the reaper as a query taking only @threshold and @row_limit, so the
// generated code does not expose the status list either; reflecting over
// internal/db/sqlc would assert nothing about the set.
//
// A behavioural test cannot cover this either. It would need a real Postgres, a
// status that is NOT in the set to insert, and the whole point of the branch is
// that no such status exists yet. Asserting against a status Phase 1 has not
// added would mean writing a test against code nobody has written.
//
// That leaves reading the two files and asserting on their content, which is
// what this does. It is exact about what it reads: SQL line comments are
// stripped before any predicate is extracted, so prose about the guard cannot
// satisfy it and editing that prose cannot break it; sets are compared as sets,
// so reordering or reformatting a predicate is not drift. What it catches is
// the one thing that matters — a status literal entering or leaving either
// guard.
//
// Precedent for asserting on repo source from a Go test:
// apps/api/tests/contract/gh402_site_delete_reclaim_structure_test.go.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// Both files live under apps/api/db; this package is apps/api/internal/update.
const (
	updatesQueryPath = "../../db/query/updates.sql"
	schemaPath       = "../../db/schema.sql"

	// The two guards, by the names they are declared under.
	staleTasksQueryName = "ListStaleUpdateTasks"
	inflightIndexName   = "update_tasks_inflight_target_idx"
)

var (
	// statusInRe matches a `status IN ('a', 'b')` predicate, capturing the
	// literal list. Applied only to comment-stripped SQL.
	statusInRe = regexp.MustCompile(`(?is)\bstatus\s+IN\s*\(([^)]*)\)`)
	// sqlStringRe pulls single-quoted literals out of a captured IN list.
	sqlStringRe = regexp.MustCompile(`'([^']*)'`)
	// queryHeaderRe matches a sqlc query header at the start of a line.
	queryHeaderRe = regexp.MustCompile(`(?m)^--\s*name:\s*(\w+)\s`)
	// lineCommentRe matches a SQL line comment through end of line.
	lineCommentRe = regexp.MustCompile(`--[^\n]*`)
)

// readSQL loads one of the two SQL files. A missing or unreadable file fails
// the test rather than silently checking nothing.
func readSQL(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("read %s: %v (the guard cannot check what it cannot read)", path, err)
	}
	if len(b) == 0 {
		t.Fatalf("read %s: file is empty", path)
	}
	return string(b)
}

// stripSQLComments removes every SQL line comment. Every predicate this file
// extracts is extracted from the result, so a comment that merely QUOTES the
// status set can neither satisfy a check nor break one. db/query/updates.sql
// contains exactly that case: CreateUpdateTask's doc comment spells out
// "WHERE status IN\n-- ('pending','running')" across two lines, which a naive
// scan would happily match instead of the real ON CONFLICT predicate below it.
func stripSQLComments(sql string) string {
	return lineCommentRe.ReplaceAllString(sql, "")
}

// statusSets returns the literal list of every `status IN (...)` predicate in
// the given SQL, in source order.
func statusSets(sql string) [][]string {
	var out [][]string
	for _, m := range statusInRe.FindAllStringSubmatch(sql, -1) {
		var lits []string
		for _, lm := range sqlStringRe.FindAllStringSubmatch(m[1], -1) {
			lits = append(lits, lm[1])
		}
		out = append(out, lits)
	}
	return out
}

// sameSet compares two status lists as SETS: order and whitespace in the SQL
// are not the invariant, membership is. A predicate rewritten as
// ('running','pending') is the same guard and must stay green.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// intersectsInFlight reports whether a status set shares any member with
// InFlightTaskStatuses. This is the sweep's trigger: a predicate that names
// 'pending' or 'running' at all is an in-flight predicate, whatever else it has
// grown, and must therefore be the whole set and nothing more.
func intersectsInFlight(set []string) bool {
	for _, s := range set {
		for _, want := range InFlightTaskStatuses {
			if s == want {
				return true
			}
		}
	}
	return false
}

func inFlightWant() []string { return InFlightTaskStatuses[:] }

// namedQuery returns the body of one sqlc query: from its `-- name:` header to
// the next header, or to EOF.
func namedQuery(t *testing.T, sql, name string) string {
	t.Helper()
	locs := queryHeaderRe.FindAllStringSubmatchIndex(sql, -1)
	for i, loc := range locs {
		if sql[loc[2]:loc[3]] != name {
			continue
		}
		end := len(sql)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		return sql[loc[0]:end]
	}
	t.Fatalf("no `-- name: %s` query in %s — it was renamed or removed, and the guard that depended on it is gone", name, updatesQueryPath)
	return ""
}

// ---------------------------------------------------------------------------
// Guard 1: the stale-task reaper.
// ---------------------------------------------------------------------------

// TestReaperSweepMatchesInFlightStatuses pins ListStaleUpdateTasks' status set
// to InFlightTaskStatuses.
//
// The failure this prevents is not subtle, it is just invisible until it has
// already happened: the reaper marks everything it selects as failed after
// staleTaskThreshold (45m, worker.go), it is registered with RunOnStart
// (cmd/wpmgr/main.go), and it sweeps every tenant under app.agent. A status
// that means "not yet due" landing in this set means the next deploy fails
// every such task across the whole fleet within seconds of boot, with no error
// and no operator action.
func TestReaperSweepMatchesInFlightStatuses(t *testing.T) {
	body := stripSQLComments(namedQuery(t, readSQL(t, updatesQueryPath), staleTasksQueryName))

	sets := statusSets(body)
	if len(sets) == 0 {
		t.Fatalf("%s has no `status IN (...)` predicate at all — the reaper no longer filters by status, so it sweeps every row it can see", staleTasksQueryName)
	}
	for _, got := range sets {
		if !sameSet(got, inFlightWant()) {
			t.Errorf("%s selects status IN %v, want exactly %v (update.InFlightTaskStatuses).\n"+
				"The reaper terminalizes everything it selects. Adding a status here fails those tasks fleet-wide on the next deploy (RunOnStart); removing one strands stuck tasks in the dedup index forever.\n"+
				"If this set genuinely must change, change update.InFlightTaskStatuses and both SQL copies together.",
				staleTasksQueryName, got, inFlightWant())
		}
	}
}

// ---------------------------------------------------------------------------
// Guard 2: the cross-run dedup partial unique index.
// ---------------------------------------------------------------------------

// TestInflightDedupIndexMatchesInFlightStatuses pins
// update_tasks_inflight_target_idx's WHERE predicate to InFlightTaskStatuses.
//
// The failure this prevents runs the other way from the reaper's: a status in
// this predicate holds the (tenant, site, target_type, target_slug) slot, and
// CreateUpdateTask's ON CONFLICT ... DO NOTHING then silently drops any other
// task for that target. A task that is not actually being worked on sitting in
// this index blocks real, immediate work, and the drop looks identical to the
// legitimate "already in flight" skip.
func TestInflightDedupIndexMatchesInFlightStatuses(t *testing.T) {
	schema := stripSQLComments(readSQL(t, schemaPath))

	// Comments are already stripped, so the index name can only match its real
	// declaration — schema.sql discusses this index in prose directly above it.
	declRe := regexp.MustCompile(`(?is)CREATE\s+UNIQUE\s+INDEX\s+` + regexp.QuoteMeta(inflightIndexName) + `\b[^;]*;`)
	decls := declRe.FindAllString(schema, -1)
	if len(decls) != 1 {
		t.Fatalf("found %d declarations of %s in %s, want exactly 1 — the authoritative cross-run dedup guard is missing or duplicated", len(decls), inflightIndexName, schemaPath)
	}

	sets := statusSets(decls[0])
	if len(sets) == 0 {
		t.Fatalf("%s has no `status IN (...)` predicate — it is no longer partial, so every terminal task keeps holding its (tenant, site, target) slot and no target can ever be updated twice", inflightIndexName)
	}
	for _, got := range sets {
		if !sameSet(got, inFlightWant()) {
			t.Errorf("%s is partial on status IN %v, want exactly %v (update.InFlightTaskStatuses).\n"+
				"A status in this predicate occupies the (tenant, site, target_type, target_slug) slot and makes CreateUpdateTask's ON CONFLICT DO NOTHING drop any other task for that target.\n"+
				"This predicate is also CreateUpdateTask's ON CONFLICT arbiter: the two must stay identical or the insert cannot resolve an arbiter.",
				inflightIndexName, got, inFlightWant())
		}
	}
}

// ---------------------------------------------------------------------------
// Sweep: every OTHER in-flight predicate on update_tasks.
// ---------------------------------------------------------------------------

// tableRefRe matches the table reference that gives a SQL scope its subject.
var tableRefRe = regexp.MustCompile(`(?is)\b(?:FROM|JOIN|UPDATE|INSERT\s+INTO)\s+(update_tasks|update_runs)\b`)

// parenScopes returns every balanced parenthesised range in sql as [open,
// close] index pairs. Parens inside single-quoted literals are ignored so a
// status value containing one cannot desynchronise the stack.
func parenScopes(sql string) [][2]int {
	var stack []int
	var out [][2]int
	inStr := false
	for i := 0; i < len(sql); i++ {
		switch sql[i] {
		case '\'':
			inStr = !inStr
		case '(':
			if !inStr {
				stack = append(stack, i)
			}
		case ')':
			if !inStr && len(stack) > 0 {
				open := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				out = append(out, [2]int{open, i})
			}
		}
	}
	return out
}

// attributeTable answers the only question that matters for the sweep: which
// table does THIS predicate constrain?
//
// Judging by which tables the whole statement mentions is too coarse — a task
// predicate inside a statement that also selects from update_runs escapes
// inspection entirely, and mixed statements already exist in this file.
// Judging by the nearest preceding table reference is wrong for a different
// reason: in ListUpdateRunsWithCounts the LATERAL's `FROM update_tasks t` sits
// BELOW the `count(*) FILTER (WHERE status IN ...)` it governs, so "nearest
// preceding" attributes that predicate to update_runs.
//
// Scope nesting is what actually decides it. Walk outward from the predicate
// through the parenthesised scopes that strictly contain it, innermost first,
// and stop at the first scope that names a table: that scope is the predicate's
// query, and its table is the predicate's subject. A FILTER(...) with no table
// of its own inherits the LATERAL subquery around it; a top-level WHERE
// inherits the statement.
//
// A scope naming BOTH tables cannot be resolved by static reading, so this
// returns an error and the caller fails loudly. Defaulting to skip there is
// exactly the hole this replaced: a predicate the guard cannot classify is the
// one most worth a human's attention, not the one to wave through.
func attributeTable(stmt string, predStart, predEnd int) (string, error) {
	type scope struct{ lo, hi int }
	var cands []scope
	for _, p := range parenScopes(stmt) {
		if p[0] < predStart && p[1] >= predEnd {
			cands = append(cands, scope{p[0], p[1]})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		return (cands[i].hi - cands[i].lo) < (cands[j].hi - cands[j].lo)
	})
	// The statement itself is the outermost scope: a top-level WHERE sits in no
	// parens at all.
	cands = append(cands, scope{0, len(stmt)})

	for _, c := range cands {
		refs := map[string]bool{}
		for _, m := range tableRefRe.FindAllStringSubmatch(stmt[c.lo:c.hi], -1) {
			refs[m[1]] = true
		}
		switch len(refs) {
		case 0:
			continue // no subject at this level; widen
		case 1:
			for tbl := range refs {
				return tbl, nil
			}
		default:
			return "", fmt.Errorf("innermost enclosing scope with a table reference names both update_tasks and update_runs, so the predicate's subject cannot be read statically")
		}
	}
	return "", fmt.Errorf("no FROM/JOIN/UPDATE/INSERT INTO reference in any enclosing scope")
}

// TestEveryUpdateTasksInFlightPredicateMatchesInFlightStatuses is the reason a
// third copy cannot appear unnoticed. The two guards above are the dangerous
// ones, but db/query/updates.sql repeats the same set in several more queries,
// and each repeat is another way for the invariant to drift.
//
// Scope is narrow in two ways, because a guard that reddens correct work gets
// deleted:
//
//   - Only predicates attributed to update_tasks are checked, and attribution
//     is per PREDICATE, by scope (see attributeTable), not per statement. Run
//     statuses are a different axis that happens to reuse the spellings
//     'pending' and 'running', so a legitimate change to a run-status predicate
//     must not be reported as task drift — but a task predicate must not escape
//     merely by sharing a statement with update_runs.
//   - Only predicates that INTERSECT InFlightTaskStatuses are checked. A
//     predicate over a disjoint set — ('failed','rolled_back') in
//     ListUpdateRunsWithCounts today, or a future status filtered on its own —
//     is not a copy of this invariant and is left alone.
//
// What survives both filters is exactly "a predicate that claims to be the
// in-flight set for update_tasks", and that must be the in-flight set.
func TestEveryUpdateTasksInFlightPredicateMatchesInFlightStatuses(t *testing.T) {
	raw := readSQL(t, updatesQueryPath)
	locs := queryHeaderRe.FindAllStringSubmatchIndex(raw, -1)
	if len(locs) == 0 {
		t.Fatalf("no `-- name:` queries found in %s — the sweep would pass by checking nothing", updatesQueryPath)
	}

	checked := 0
	attributed := map[string]bool{}
	for i, loc := range locs {
		name := raw[loc[2]:loc[3]]
		end := len(raw)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		stmt := stripSQLComments(raw[loc[0]:end])

		for _, m := range statusInRe.FindAllStringSubmatchIndex(stmt, -1) {
			var lits []string
			for _, lm := range sqlStringRe.FindAllStringSubmatch(stmt[m[2]:m[3]], -1) {
				lits = append(lits, lm[1])
			}
			if !intersectsInFlight(lits) {
				continue
			}
			tbl, err := attributeTable(stmt, m[0], m[1])
			if err != nil {
				t.Errorf("%s: cannot attribute `status IN %v` to a table: %v.\n"+
					"This predicate names an in-flight status, so it may be a copy of update.InFlightTaskStatuses, and the guard will not wave through what it cannot classify. Split the scope or qualify the column so its subject is readable.",
					name, lits, err)
				continue
			}
			if tbl != "update_tasks" {
				continue
			}
			checked++
			attributed[name] = true
			if !sameSet(lits, inFlightWant()) {
				t.Errorf("%s: status IN %v on update_tasks, want exactly %v (update.InFlightTaskStatuses).\n"+
					"This predicate names an in-flight status, so it is a copy of that set and must be the whole set.",
					name, lits, inFlightWant())
			}
		}
	}

	// A guard that finds nothing must go red. If the regexes, the query-header
	// format or scope attribution ever stop matching, the loop above becomes a
	// no-op that reports success.
	if checked == 0 {
		t.Fatalf("found no update_tasks in-flight status predicate anywhere in %s — the sweep matched nothing and would have passed vacuously", updatesQueryPath)
	}
	// The two named guards must be inside the sweep's reach. This is what
	// catches attribution silently narrowing to nothing useful while still
	// finding something somewhere.
	if !attributed[staleTasksQueryName] {
		t.Errorf("%s contributed no update_tasks in-flight predicate to the sweep; attribution no longer reaches the reaper", staleTasksQueryName)
	}
	if !attributed["CreateUpdateTask"] {
		t.Errorf("CreateUpdateTask contributed no update_tasks in-flight predicate to the sweep; attribution no longer reaches the ON CONFLICT arbiter that must match %s", inflightIndexName)
	}
}
