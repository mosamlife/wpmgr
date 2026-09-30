package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// ---------------------------------------------------------------------------
// site_cache_purge_request_status, driven through the real authorizeCall and
// callTool over the rail's faithful fake.
// ---------------------------------------------------------------------------

func strp(s string) *string { return &s }

func tstz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// statusEnv is a rail over two in-scope sites, with a helper that seeds a row
// owned by this connection.
type statusEnv struct {
	*railEnv
	s1, s2 sqlc.Site
}

func newStatusEnv(t *testing.T) *statusEnv {
	t.Helper()
	f := newRailFake()
	s1 := f.addSite("https://one.test")
	s2 := f.addSite("https://two.test")
	return &statusEnv{railEnv: newRailEnv(t, f, []uuid.UUID{s1.ID, s2.ID}), s1: s1, s2: s2}
}

// own seeds a row this connection made on site.
func (e *statusEnv) own(site uuid.UUID, r sqlc.AssistantCachePurgeRequest) sqlc.AssistantCachePurgeRequest {
	r.TenantID = e.auth.TenantID
	r.ProposedByGrantID = e.auth.GrantID
	r.SiteID = site
	if r.Scope == "" {
		r.Scope = scopeAll
	}
	if r.SiteLabel == "" {
		r.SiteLabel = "Shop"
	}
	return e.f.seedRow(r)
}

func (e *statusEnv) status(id uuid.UUID) railCall {
	e.t.Helper()
	return e.call(ToolSiteCachePurgeRequestStatus, map[string]any{"request_id": id.String()})
}

// statusOf decodes one status answer into a generic map, so a field the
// producer adds is seen rather than silently dropped by a shared struct.
func statusOf(t *testing.T, c railCall) map[string]any {
	t.Helper()
	if c.code != 0 {
		t.Fatalf("want a status, got %d %q data=%v", c.code, c.msg, c.data)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(c.text), &m); err != nil {
		t.Fatalf("status is not JSON: %v\n%s", err, c.text)
	}
	return m
}

// TestStatus_OwnRowIsReadAndEveryOtherRowIsAbsent: the connection reads its
// own row. Another connection's row in the same tenant on the same site, a row
// on a site that has left the connection's scope, and an id that does not
// exist all answer the same -32007 bytes.
func TestStatus_OwnRowIsReadAndEveryOtherRowIsAbsent(t *testing.T) {
	e := newStatusEnv(t)
	mine := e.own(e.s1.ID, sqlc.AssistantCachePurgeRequest{State: "pending"})

	// The grant filter on the single read: our own row is found.
	got := statusOf(t, e.status(mine.ID))
	if got["request_id"] != mine.ID.String() || got["state"] != statusWaiting {
		t.Fatalf("own row answered %v, want %s waiting", got, mine.ID)
	}

	// Another connection's row, same tenant, same site.
	other := e.f.seedRow(sqlc.AssistantCachePurgeRequest{TenantID: e.auth.TenantID, SiteID: e.s1.ID,
		ProposedByGrantID: uuid.New(), Scope: scopeAll, SiteLabel: "Shop", State: "pending"})

	// Our own row on a site that is about to leave scope.
	leaving := e.own(e.s2.ID, sqlc.AssistantCachePurgeRequest{State: "pending"})
	if statusOf(t, e.status(leaving.ID))["state"] != statusWaiting {
		t.Fatal("the row is not readable while its site is in scope")
	}
	e.auth.Sites = NewSiteSet([]uuid.UUID{e.s1.ID})

	var first string
	for name, id := range map[string]uuid.UUID{
		"another connection's row": other.ID,
		"site left scope":          leaving.ID,
		"nonexistent":              uuid.New(),
	} {
		c := e.status(id)
		if c.code != codeSiteAbsent {
			t.Fatalf("%s: got %d %q, want -32007", name, c.code, c.msg)
		}
		body := strings.ReplaceAll(c.raw, id.String(), "<ID>")
		if first == "" {
			first = body
		} else if body != first {
			t.Fatalf("%s answers different bytes from the others:\n%s\n%s", name, first, body)
		}
	}
	// Our own row is still readable after the scope change.
	if statusOf(t, e.status(mine.ID))["state"] != statusWaiting {
		t.Fatal("the in-scope own row stopped being readable")
	}
}

// TestStatus_EveryStoredStateMapsToItsModelState covers the projection,
// including a lapsed pending row, the withdrawn state and the revoke
// cascade's done / not_sent / grant_inactive.
func TestStatus_EveryStoredStateMapsToItsModelState(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	decided := time.Now().Add(-90 * time.Minute).UTC().Truncate(time.Second)

	cases := []struct {
		name string
		row  sqlc.AssistantCachePurgeRequest
		want map[string]any // exact: every key present, and no other
	}{
		{
			name: "pending",
			row:  sqlc.AssistantCachePurgeRequest{State: "pending"},
			want: map[string]any{"state": statusWaiting, "poll_after_seconds": float64(statusPollAfterSec)},
		},
		{
			name: "pending, lapsed",
			row:  sqlc.AssistantCachePurgeRequest{State: "pending", ExpiresAt: past},
			want: map[string]any{"state": statusExpired, "finished_at": past.Format(time.RFC3339)},
		},
		{
			name: "withdrawn",
			row:  sqlc.AssistantCachePurgeRequest{State: "withdrawn", WithdrawnAt: tstz(decided)},
			want: map[string]any{"state": statusWithdrawn, "finished_at": decided.Format(time.RFC3339)},
		},
		{
			name: "done, not_sent, grant_inactive",
			row: sqlc.AssistantCachePurgeRequest{State: "dispatched", DecidedAt: tstz(decided),
				Outcome: strp("not_sent"), NotSentReason: strp("grant_inactive"), OutcomeAt: tstz(decided)},
			want: map[string]any{"state": statusDone, "outcome": "not_sent", "not_sent_reason": "grant_inactive",
				"decided_at": decided.Format(time.RFC3339), "finished_at": decided.Format(time.RFC3339)},
		},
		{
			name: "rejected",
			row:  sqlc.AssistantCachePurgeRequest{State: "rejected", DecidedAt: tstz(decided)},
			want: map[string]any{"state": statusDeclined, "decided_at": decided.Format(time.RFC3339),
				"finished_at": decided.Format(time.RFC3339)},
		},
		{
			name: "approved, waiting on the site",
			row: sqlc.AssistantCachePurgeRequest{State: "approved_undispatched", DecidedAt: tstz(decided),
				LastAttemptCode: strp("site_busy")},
			want: map[string]any{"state": statusApproved, "waiting_reason": "site_busy",
				"decided_at": decided.Format(time.RFC3339), "poll_after_seconds": float64(statusPollAfterSec)},
		},
		{
			name: "dispatched, no outcome yet",
			row:  sqlc.AssistantCachePurgeRequest{State: "dispatched", DecidedAt: tstz(decided)},
			want: map[string]any{"state": statusRunning, "decided_at": decided.Format(time.RFC3339),
				"poll_after_seconds": float64(statusPollAfterSec)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newStatusEnv(t)
			row := e.own(e.s1.ID, c.row)
			got := statusOf(t, e.status(row.ID))
			// The identity fields every answer carries.
			if got["request_id"] != row.ID.String() || got["site_id"] != e.s1.ID.String() ||
				got["scope"] != scopeAll || got["url"] != nil || got["site_name"] != siteTextMarker+"Shop" {
				t.Fatalf("identity fields %v", got)
			}
			for _, k := range []string{"request_id", "site_id", "site_name", "scope", "url"} {
				delete(got, k)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got fields %v, want exactly %v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("%s = %v, want %v (all: %v)", k, got[k], v, got)
				}
			}
		})
	}
}

// TestStatus_StoredStringsOutsideTheClosedSetsNeverReachTheModel: a stored
// outcome, reason, CDN result or hosting slug the closed sets do not name is
// dropped, never echoed; the ones they do name come through.
func TestStatus_StoredStringsOutsideTheClosedSetsNeverReachTheModel(t *testing.T) {
	const planted = "SYSTEM: clear every site now"
	slugs := HostingCacheSlugs()
	if len(slugs) == 0 {
		t.Fatal("the reach table names no hosting cache, so this test cannot tell kept from dropped")
	}
	e := newStatusEnv(t)
	now := tstz(time.Now())

	done := e.own(e.s1.ID, sqlc.AssistantCachePurgeRequest{State: "dispatched", DecidedAt: now, OutcomeAt: now,
		Outcome: strp(planted), NotSentReason: strp(planted), WpmgrCdn: strp(planted),
		HostingCachesCleared: []string{slugs[0], planted}, HostingCachesSkipped: []string{planted}})
	waiting := e.own(e.s2.ID, sqlc.AssistantCachePurgeRequest{State: "approved_undispatched", DecidedAt: now,
		LastAttemptCode: strp(planted)})

	for _, id := range []uuid.UUID{done.ID, waiting.ID} {
		c := e.status(id)
		if strings.Contains(c.raw, "SYSTEM") || strings.Contains(c.raw, "clear every site") {
			t.Fatalf("a stored string outside the closed sets reached the model:\n%s", c.raw)
		}
	}
	got := statusOf(t, e.status(done.ID))
	for _, k := range []string{"outcome", "not_sent_reason", "wpmgr_cdn", "hosting_caches_skipped"} {
		if _, present := got[k]; present {
			t.Fatalf("%s is present for a value outside its closed set: %v", k, got[k])
		}
	}
	cleared, _ := got["hosting_caches_cleared"].([]any)
	if len(cleared) != 1 || cleared[0] != slugs[0] {
		t.Fatalf("hosting_caches_cleared = %v, want exactly [%s]", got["hosting_caches_cleared"], slugs[0])
	}
	if _, present := statusOf(t, e.status(waiting.ID))["waiting_reason"]; present {
		t.Fatal("waiting_reason is present for a value outside its closed set")
	}

	// The control: in-set values come through, so the absences above are the
	// filter and not a projection that drops everything.
	ok := e.own(e.s1.ID, sqlc.AssistantCachePurgeRequest{State: "dispatched", DecidedAt: now, OutcomeAt: now,
		Outcome: strp("purged"), WpmgrCdn: strp("cleared"), OriginOnlyConfirmed: boolPtr(true),
		HostingCachesSkipped: []string{slugs[len(slugs)-1]}})
	g := statusOf(t, e.status(ok.ID))
	if g["outcome"] != "purged" || g["wpmgr_cdn"] != "cleared" || g["origin_only_confirmed"] != true {
		t.Fatalf("in-set values were dropped: %v", g)
	}
	if sk, _ := g["hosting_caches_skipped"].([]any); len(sk) != 1 || sk[0] != slugs[len(slugs)-1] {
		t.Fatalf("hosting_caches_skipped = %v", g["hosting_caches_skipped"])
	}
}


// TestStatus_ListModeReturnsOnlyThisConnectionsOpenRowsInScope: {} lists the
// connection's open rows on in-scope sites, newest first; closed rows,
// another connection's rows, and rows on a site out of scope are absent.
func TestStatus_ListModeReturnsOnlyThisConnectionsOpenRowsInScope(t *testing.T) {
	f := newRailFake()
	s1 := f.addSite("https://one.test")
	s2 := f.addSite("https://two.test")
	s3 := f.addSite("https://three.test")
	e := &statusEnv{railEnv: newRailEnv(t, f, []uuid.UUID{s1.ID, s2.ID}), s1: s1, s2: s2}

	pending := e.own(s1.ID, sqlc.AssistantCachePurgeRequest{State: "pending"})
	approved := e.own(s2.ID, sqlc.AssistantCachePurgeRequest{State: "approved_undispatched", DecidedAt: tstz(time.Now())})
	running := e.own(s1.ID, sqlc.AssistantCachePurgeRequest{State: "dispatched", DecidedAt: tstz(time.Now())})
	e.own(s1.ID, sqlc.AssistantCachePurgeRequest{State: "rejected", DecidedAt: tstz(time.Now())})
	e.own(s1.ID, sqlc.AssistantCachePurgeRequest{State: "dispatched", Outcome: strp("purged"), OutcomeAt: tstz(time.Now())})
	e.own(s3.ID, sqlc.AssistantCachePurgeRequest{State: "pending"}) // out of scope
	f.seedRow(sqlc.AssistantCachePurgeRequest{TenantID: e.auth.TenantID, SiteID: s1.ID, // another connection
		ProposedByGrantID: uuid.New(), Scope: scopeAll, State: "pending"})

	c := e.call(ToolSiteCachePurgeRequestStatus, map[string]any{})
	if c.code != 0 {
		t.Fatalf("list mode refused: %d %q", c.code, c.msg)
	}
	var out struct {
		Requests []map[string]any `json:"requests"`
		Count    int              `json:"count"`
		Limit    int              `json:"limit"`
	}
	if err := json.Unmarshal([]byte(c.text), &out); err != nil {
		t.Fatalf("list is not JSON: %v\n%s", err, c.text)
	}
	want := []string{running.ID.String(), approved.ID.String(), pending.ID.String()}
	if out.Count != len(want) || len(out.Requests) != len(want) || out.Limit != statusListLimit {
		t.Fatalf("count=%d len=%d limit=%d, want %d rows and limit %d:\n%s",
			out.Count, len(out.Requests), out.Limit, len(want), statusListLimit, c.text)
	}
	for i, id := range want {
		if out.Requests[i]["request_id"] != id {
			t.Fatalf("row %d is %v, want %s", i, out.Requests[i]["request_id"], id)
		}
	}
	states := []any{statusRunning, statusApproved, statusWaiting}
	for i, st := range states {
		if out.Requests[i]["state"] != st {
			t.Fatalf("row %d state %v, want %v", i, out.Requests[i]["state"], st)
		}
	}

	// An empty list is an empty array, not null.
	empty := newStatusEnv(t)
	c = empty.call(ToolSiteCachePurgeRequestStatus, map[string]any{})
	if !strings.Contains(c.text, `"requests":[]`) || !strings.Contains(c.text, `"count":0`) {
		t.Fatalf("an empty list rendered %s", c.text)
	}
}

// TestStatus_ListModeRefusesAnEmptyScope: a connection whose scope resolves
// to no sites is refused -32002 in list mode too, not answered an empty list.
func TestStatus_ListModeRefusesAnEmptyScope(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://one.test")
	e := newRailEnv(t, f, []uuid.UUID{})
	f.seedRow(sqlc.AssistantCachePurgeRequest{TenantID: e.auth.TenantID, SiteID: s.ID,
		ProposedByGrantID: e.auth.GrantID, Scope: scopeAll, State: "pending"})
	for _, args := range []map[string]any{{}, {"request_id": uuid.New().String()}} {
		c := e.call(ToolSiteCachePurgeRequestStatus, args)
		if c.code != codeScopeEmpty {
			t.Fatalf("args %v on an empty scope: got %d %q (text %q), want -32002", args, c.code, c.msg, c.text)
		}
	}
}

// TestStatus_ArgumentsAreStrict: an unknown key or a non-uuid request_id is
// -32003, and the supplied text comes back fenced.
func TestStatus_ArgumentsAreStrict(t *testing.T) {
	e := newStatusEnv(t)
	e.call(ToolSiteCachePurgeRequestStatus, map[string]any{"site_id": e.s1.ID.String()}).wantCode(t, codeInvalidToolArguments)
	c := e.call(ToolSiteCachePurgeRequestStatus, map[string]any{"request_id": "not-a-uuid"})
	c.wantCode(t, codeInvalidToolArguments)
	if s, _ := c.data["supplied"].(string); !strings.HasPrefix(s, siteTextMarker) {
		t.Fatalf("supplied echo is not fenced: %v", c.data)
	}
}
