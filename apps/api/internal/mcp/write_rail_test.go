package mcp

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// ---------------------------------------------------------------------------
// G2: the site's own address, for BOTH scopes.
// ---------------------------------------------------------------------------

// TestRail_G2_SiteAddressUnusableIsMinus32014ForBothScopes: a site whose
// stored address has no host WPMgr can dial answers -32014 for `all` and for
// `url`, never -32603, with no request row and a denial row naming the
// reason.
func TestRail_G2_SiteAddressUnusableIsMinus32014ForBothScopes(t *testing.T) {
	host256 := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." +
		strings.Repeat("c", 63) + "." + strings.Repeat("d", 62) + ".e" // 256 bytes
	if len(host256) != 256 {
		t.Fatalf("fixture host is %d bytes, want 256", len(host256))
	}
	cases := map[string]string{
		"no HostKey":        "https://bü_cher.test",
		"256-byte host":     "https://" + host256,
		"query in address":  "https://x.test/?p=1",
		"fragment":          "https://x.test/#top",
		"another scheme":    "ftp://x.test",
		"encoded host byte": "https://x%20y.test",
	}
	for name, stored := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRailFake()
			s := f.addSite(stored)
			env := newRailEnv(t, f, nil)
			for _, args := range []map[string]any{allReq(s.ID), urlReq(s.ID, "https://x.test/a")} {
				c := env.call(ToolSiteCachePurgeRequest, args)
				if c.code != codeSiteAddressUnusable {
					t.Fatalf("scope %v on %q: got %d %q, want -32014", args["scope"], stored, c.code, c.msg)
				}
				if c.msg != siteAddressUnusableMessage {
					t.Fatalf("message %q is not the constant", c.msg)
				}
				ev, ok := env.rec.last(audit.ActionMCPToolDenied)
				if !ok || ev.Metadata["refusal_reason"] != string(reasonSiteAddressUnusable) {
					t.Fatalf("denial row missing or wrong: %+v", ev.Metadata)
				}
			}
			if n := len(f.rowsFor(env.auth.GrantID)); n != 0 {
				t.Fatalf("%d request rows written for an unusable address, want 0", n)
			}
			if f.insertCalls != 0 {
				t.Fatalf("the insert ran %d times", f.insertCalls)
			}
		})
	}
	// Still accepted: 255 bytes, an underscore in an ASCII host, IPv6.
	for _, stored := range []string{"https://" + host256[:255], "https://a_b.test", "https://[::1]", "https://[2001:db8::1]"} {
		f := newRailFake()
		s := f.addSite(stored)
		env := newRailEnv(t, f, nil)
		env.call(ToolSiteCachePurgeRequest, allReq(s.ID)).created(t)
	}
}

// TestRail_G2_IDNSiteStoresThePunycodeHostForBothScopes: a whole-site
// request on an IDN site stores the Punycode host and no url; a page request
// stores the same host, rebuilt from the site row.
func TestRail_G2_IDNSiteStoresThePunycodeHostForBothScopes(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://bücher.de/shop")
	env := newRailEnv(t, f, nil)

	all := env.call(ToolSiteCachePurgeRequest, allReq(s.ID)).created(t)
	if all.URL != nil {
		t.Fatalf("a whole-site request returned url %q", *all.URL)
	}
	rows := f.rowsFor(env.auth.GrantID)
	if len(rows) != 1 || rows[0].SiteHost != "xn--bcher-kva.de" || rows[0].Url != nil {
		t.Fatalf("whole-site row = %+v, want site_host xn--bcher-kva.de and no url", rows)
	}

	// A page request from another connection on the same site.
	env2 := newRailEnv(t, f, []uuid.UUID{s.ID})
	env2.auth.TenantID = env.auth.TenantID
	page := env2.call(ToolSiteCachePurgeRequest, urlReq(s.ID, "https://xn--bcher-kva.de/shop/sale/")).created(t)
	if got := unfence(t, *page.URL); got != "https://xn--bcher-kva.de/shop/sale/" {
		t.Fatalf("stored url %q", got)
	}
	prow := f.rowsFor(env2.auth.GrantID)
	if len(prow) != 1 || prow[0].SiteHost != rows[0].SiteHost {
		t.Fatalf("the two scopes derived different hosts: %+v vs %q", prow, rows[0].SiteHost)
	}
	// The raw IDN spelling is refused at the grammar.
	env2.call(ToolSiteCachePurgeRequest, urlReq(s.ID, "https://bücher.de/shop/sale/")).wantCode(t, codeInvalidToolArguments)
}

// ---------------------------------------------------------------------------
// Arguments and the URL grammar.
// ---------------------------------------------------------------------------

func TestRail_ArgumentsAreRefusedAsMinus32003(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://x.test/blog")
	env := newRailEnv(t, f, nil)
	id := s.ID.String()
	bad := map[string]any{
		"URL scope":      map[string]any{"site_id": id, "scope": "URL", "url": "https://x.test/blog/a"},
		"ALL scope":      map[string]any{"site_id": id, "scope": "ALL"},
		"everything":     map[string]any{"site_id": id, "scope": "everything"},
		"url with all":   map[string]any{"site_id": id, "scope": "all", "url": "https://x.test/blog/a"},
		"missing url":    map[string]any{"site_id": id, "scope": "url"},
		"url all":        urlReq(s.ID, "all"),
		"url ALL":        urlReq(s.ID, "ALL"),
		"RLO":            urlReq(s.ID, "https://x.test/blog/a‮b"),
		"ZWSP":           urlReq(s.ID, "https://x.test/blog/a​b"),
		"space":          urlReq(s.ID, "https://x.test/blog/a b"),
		"non-ascii":      urlReq(s.ID, "https://x.test/blog/é"),
		"backslash":      urlReq(s.ID, `https://x.test/blog/a\b`),
		"query":          urlReq(s.ID, "https://x.test/blog/a?utm=x"),
		"bare ?":         urlReq(s.ID, "https://x.test/blog/a?"),
		"bare #":         urlReq(s.ID, "https://x.test/blog/a#"),
		"fragment":       urlReq(s.ID, "https://x.test/blog/a#b"),
		"%2f":            urlReq(s.ID, "https://x.test/blog/a%2f..%2fb"),
		"%2F upper":      urlReq(s.ID, "https://x.test/blog/a%2F%2E%2E%2Fb"),
		"%5c":            urlReq(s.ID, "https://x.test/blog/a%5cb"),
		"dotdot":         urlReq(s.ID, "https://x.test/blog/a/../b"),
		"encoded dotdot": urlReq(s.ID, "https://x.test/blog/a/%2e%2e/b"),
		"dot":            urlReq(s.ID, "https://x.test/blog/./b"),
		"decoded RLO":    urlReq(s.ID, "https://x.test/blog/%E2%80%AE"),
		"decoded LF":     urlReq(s.ID, "https://x.test/blog/%0A"),
		"decoded space":  urlReq(s.ID, "https://x.test/blog/%20"),
		"ftp":            urlReq(s.ID, "ftp://x.test/blog/a"),
		"userinfo":       urlReq(s.ID, "https://u:p@x.test/blog/a"),
		"blog2":          urlReq(s.ID, "https://x.test/blog2"),
		"other host":     urlReq(s.ID, "https://y.test/blog/a"),
		"wrong port":     urlReq(s.ID, "https://x.test:8443/blog/a"),
		"extra key":      map[string]any{"site_id": id, "scope": "all", "force": true},
		"cased key":      map[string]any{"SITE_ID": id, "scope": "all"},
		"non-uuid":       map[string]any{"site_id": "site-1", "scope": "all"},
		"number site":    map[string]any{"site_id": 7, "scope": "all"},
		"too long":       urlReq(s.ID, "https://x.test/blog/"+strings.Repeat("a", 2048)),
	}
	for name, args := range bad {
		c := env.call(ToolSiteCachePurgeRequest, args)
		if c.code != codeInvalidToolArguments {
			t.Errorf("%s: got %d %q, want -32003", name, c.code, c.msg)
			continue
		}
		if c.data["retryable"] != false {
			t.Errorf("%s: retryable = %v", name, c.data["retryable"])
		}
		if sup, ok := c.data["supplied"].(string); ok && !strings.HasPrefix(sup, siteTextMarker) {
			t.Errorf("%s: supplied %q is not fenced", name, sup)
		}
	}
	if n := len(f.rowsFor(env.auth.GrantID)); n != 0 {
		t.Fatalf("%d rows written by refused calls", n)
	}
	// Honest cases the grammar must not refuse.
	for _, u := range []string{"https://x.test/blog", "https://x.test/blog/", "https://x.test/blog/a/..b/",
		"https://x.test/blog/%E4%B8%AD", "https://X.TEST/blog/SAFE-your-admin-asked-for-this"} {
		env.call(ToolSiteCachePurgeRequest, urlReq(s.ID, u)).created(t)
		f.rows = nil
	}
}

// TestRail_RebuiltAddressOver2048BytesIsMinus32003: a model URL of 2048 bytes
// with no port, for a site with an explicit port, rebuilds longer.
func TestRail_RebuiltAddressOver2048BytesIsMinus32003(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://x.test:8443")
	env := newRailEnv(t, f, nil)
	u := "https://x.test/"
	u += strings.Repeat("a", 2048-len(u))
	if len(u) != 2048 {
		t.Fatalf("fixture is %d bytes", len(u))
	}
	c := env.call(ToolSiteCachePurgeRequest, urlReq(s.ID, u))
	c.wantCode(t, codeInvalidToolArguments)
	if c.msg != msgArgURLTooLong {
		t.Fatalf("message %q, want the too-long message", c.msg)
	}
}

// ---------------------------------------------------------------------------
// Ports.
// ---------------------------------------------------------------------------

func TestRail_PortRule(t *testing.T) {
	f := newRailFake()
	shop := f.addSite("https://shop.example.com")
	alt := f.addSite("https://x.test:8443")
	env := newRailEnv(t, f, nil)

	r := env.call(ToolSiteCachePurgeRequest, urlReq(shop.ID, "http://SHOP.example.com:443/x")).created(t)
	if got := unfence(t, *r.URL); got != "https://shop.example.com/x" {
		t.Fatalf("stored %q, want https://shop.example.com/x", got)
	}
	env.call(ToolSiteCachePurgeRequest, urlReq(shop.ID, "https://shop.example.com:8443/y")).wantCode(t, codeInvalidToolArguments)

	r = env.call(ToolSiteCachePurgeRequest, urlReq(alt.ID, "https://x.test/x")).created(t)
	if got := unfence(t, *r.URL); got != "https://x.test:8443/x" {
		t.Fatalf("stored %q, want https://x.test:8443/x", got)
	}
}

// ---------------------------------------------------------------------------
// Ties: the three messages, each asserted exactly.
// ---------------------------------------------------------------------------

func TestRail_TieMessages(t *testing.T) {
	t.Run("(i) a longer covering path on the same port", func(t *testing.T) {
		f := newRailFake()
		root := f.addSite("https://x.test")
		shop := f.addSite("https://x.test/shop")
		env := newRailEnv(t, f, nil)
		c := env.call(ToolSiteCachePurgeRequest, urlReq(root.ID, "https://x.test/shop/a"))
		c.wantCode(t, codeInvalidToolArguments)
		if c.msg != msgTieOtherSite {
			t.Fatalf("message %q, want (i)", c.msg)
		}
		env.call(ToolSiteCachePurgeRequest, urlReq(shop.ID, "https://x.test/shop/a")).created(t)
	})
	t.Run("(ii) another port, no port written", func(t *testing.T) {
		f := newRailFake()
		a := f.addSite("https://x.test")
		b := f.addSite("https://x.test:8443")
		env := newRailEnv(t, f, nil)
		for _, id := range []uuid.UUID{a.ID, b.ID} {
			c := env.call(ToolSiteCachePurgeRequest, urlReq(id, "https://x.test/a"))
			if c.code != codeInvalidToolArguments || c.msg != msgTieWritePort {
				t.Fatalf("site %s: %d %q, want (ii)", id, c.code, c.msg)
			}
		}
		r := env.call(ToolSiteCachePurgeRequest, urlReq(a.ID, "https://x.test:443/a")).created(t)
		if got := unfence(t, *r.URL); got != "https://x.test/a" {
			t.Fatalf("stored %q", got)
		}
		env.call(ToolSiteCachePurgeRequest, urlReq(b.ID, "https://x.test:8443/a")).created(t)
	})
	t.Run("(ii) across schemes", func(t *testing.T) {
		f := newRailFake()
		h := f.addSite("http://x.test")
		s := f.addSite("https://x.test")
		env := newRailEnv(t, f, nil)
		c := env.call(ToolSiteCachePurgeRequest, urlReq(s.ID, "https://x.test/a"))
		if c.msg != msgTieWritePort {
			t.Fatalf("%d %q, want (ii)", c.code, c.msg)
		}
		r := env.call(ToolSiteCachePurgeRequest, urlReq(s.ID, "https://x.test:443/a")).created(t)
		if got := unfence(t, *r.URL); got != "https://x.test/a" {
			t.Fatalf("stored %q", got)
		}
		r = env.call(ToolSiteCachePurgeRequest, urlReq(h.ID, "https://x.test:80/a")).created(t)
		if got := unfence(t, *r.URL); got != "http://x.test/a" {
			t.Fatalf("stored %q", got)
		}
	})
	t.Run("(iii) one address listed twice", func(t *testing.T) {
		f := newRailFake()
		a := f.addSite("https://x.test")
		b := f.addSite("https://x.test/")
		env := newRailEnv(t, f, nil)
		for _, id := range []uuid.UUID{a.ID, b.ID} {
			for _, u := range []string{"https://x.test/a", "https://x.test:443/a"} {
				c := env.call(ToolSiteCachePurgeRequest, urlReq(id, u))
				if c.code != codeInvalidToolArguments || c.msg != msgTieSameAddress {
					t.Fatalf("site %s url %s: %d %q, want (iii)", id, u, c.code, c.msg)
				}
			}
		}
	})
	t.Run("a site with no HostKey never ties", func(t *testing.T) {
		f := newRailFake()
		a := f.addSite("https://x.test")
		f.addSite("https://bü_cher.test")
		env := newRailEnv(t, f, nil)
		env.call(ToolSiteCachePurgeRequest, urlReq(a.ID, "https://x.test/a")).created(t)
	})
	t.Run("60 in-scope sites: the covering site is found wherever it sorts", func(t *testing.T) {
		f := newRailFake()
		root := f.addSite("https://x.test")
		for i := 0; i < 58; i++ {
			f.addSite(fmt.Sprintf("https://other%02d.test", i))
		}
		f.addSite("https://x.test/zzz")
		env := newRailEnv(t, f, nil)
		c := env.call(ToolSiteCachePurgeRequest, urlReq(root.ID, "https://x.test/zzz/a"))
		if c.msg != msgTieOtherSite {
			t.Fatalf("%d %q, want (i)", c.code, c.msg)
		}
	})
	t.Run("no message names a host or a port", func(t *testing.T) {
		for _, m := range []string{msgTieOtherSite, msgTieWritePort, msgTieSameAddress, msgArgURLNotOnSite, siteAddressUnusableMessage} {
			if strings.Contains(m, "x.test") || strings.Contains(m, "8443") {
				t.Fatalf("message %q carries a host or port", m)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Absent, context, agent.
// ---------------------------------------------------------------------------

// TestRail_AbsentSitesAreByteIdentical: out of scope, archived and
// nonexistent answer the same bytes, with no request row.
func TestRail_AbsentSitesAreByteIdentical(t *testing.T) {
	f := newRailFake()
	in := f.addSite("https://in.test")
	out := f.addSite("https://out.test")
	arch := f.addSite("https://arch.test")
	a := f.db[arch.ID]
	a.ConnectionState = "archived"
	f.db[arch.ID] = a
	ghost := uuid.New()
	env := newRailEnv(t, f, []uuid.UUID{in.ID, arch.ID, ghost})

	var first string
	for _, id := range []uuid.UUID{out.ID, arch.ID, ghost, uuid.New()} {
		c := env.call(ToolSiteCachePurgeRequest, allReq(id))
		c.wantCode(t, codeSiteAbsent)
		body := strings.ReplaceAll(c.raw, id.String(), "<ID>")
		if first == "" {
			first = body
		} else if body != first {
			t.Fatalf("absent answers differ:\n%s\n%s", first, body)
		}
	}
	if n := len(f.rowsFor(env.auth.GrantID)); n != 0 {
		t.Fatalf("%d rows", n)
	}
	for _, p := range f.readPrincipals {
		if len(p.AllowedSiteIDs) != 1 {
			t.Fatalf("the site read ran under %v, want exactly one site", p.AllowedSiteIDs)
		}
	}
}

func TestRail_GovernedContextForbidsAtOrgAndSiteAndFailsClosed(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://x.test")
	env := newRailEnv(t, f, nil)

	env.ctx.sites[s.ID] = []string{"site cache purge"}
	c := env.call(ToolSiteCachePurgeRequest, allReq(s.ID))
	c.wantCode(t, codeToolForbiddenByContext)
	ev, _ := env.rec.last(audit.ActionMCPToolDenied)
	if ev.Metadata["refusal_reason"] != string(reasonForbiddenByContext) || ev.Metadata["matched_entry"] != "site cache purge" {
		t.Fatalf("denial row %+v", ev.Metadata)
	}

	env.ctx.sites = map[uuid.UUID][]string{}
	env.ctx.org = []string{"site_cache_purge_request"}
	env.call(ToolSiteCachePurgeRequest, allReq(s.ID)).wantCode(t, codeToolForbiddenByContext)

	env.ctx.org = nil
	env.ctx.err = errors.New("context store down")
	c = env.call(ToolSiteCachePurgeRequest, allReq(s.ID))
	c.wantCode(t, codeInternalError)
	ev, _ = env.rec.last(audit.ActionMCPToolDenied)
	if ev.Metadata["refusal_reason"] != string(reasonContextUnavailable) {
		t.Fatalf("a resolver error recorded %v, want context_unavailable", ev.Metadata["refusal_reason"])
	}
	if n := len(f.rowsFor(env.auth.GrantID)); n != 0 {
		t.Fatalf("%d rows written while forbidden or unresolvable", n)
	}
}

func TestRail_AgentFloorAndConnection(t *testing.T) {
	for _, v := range []string{"0.61.152", "", "garbage", "0.61"} {
		f := newRailFake()
		s := f.addSite("https://x.test")
		row := f.db[s.ID]
		row.AgentVersion = v
		f.db[s.ID] = row
		env := newRailEnv(t, f, nil)
		c := env.call(ToolSiteCachePurgeRequest, allReq(s.ID))
		c.wantCode(t, codeSiteAgentOutdated)
		if c.data["min_agent_version"] != MinAgentVersionForOriginOnlyPurge || !strings.Contains(c.msg, MinAgentVersionForOriginOnlyPurge) {
			t.Fatalf("version %q: the refusal does not name the floor: %q %v", v, c.msg, c.data)
		}
	}
	f := newRailFake()
	s := f.addSite("https://x.test")
	row := f.db[s.ID]
	row.ConnectionState = "offline"
	f.db[s.ID] = row
	env := newRailEnv(t, f, nil)
	c := env.call(ToolSiteCachePurgeRequest, allReq(s.ID))
	c.wantCode(t, codeSiteUnreachable)
	if c.data["retryable"] != true {
		t.Fatalf("unreachable must be retryable: %v", c.data)
	}
}

// ---------------------------------------------------------------------------
// Dedupe, the race, the in-line expiry, caps, and the audit order.
// ---------------------------------------------------------------------------

func TestRail_DedupeIsPerConnection(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://x.test")
	x := newRailEnv(t, f, nil)
	y := newRailEnv(t, f, nil)
	y.auth.TenantID = x.auth.TenantID

	first := x.call(ToolSiteCachePurgeRequest, allReq(s.ID)).created(t)
	if first.Existing {
		t.Fatal("the first request says existing")
	}
	again := x.call(ToolSiteCachePurgeRequest, allReq(s.ID)).created(t)
	if !again.Existing || again.RequestID != first.RequestID {
		t.Fatalf("a repeat gave %+v, want existing:true for %s", again, first.RequestID)
	}
	c := x.call(ToolSiteCachePurgeRequest, urlReq(s.ID, "https://x.test/a"))
	c.wantCode(t, codeRateLimited)
	if c.data["limit_scope"] != limitScopePendingForSite || c.data["request_id"] != first.RequestID || c.data["scope"] != "all" {
		t.Fatalf("pending_request_for_site data %v", c.data)
	}
	yr := y.call(ToolSiteCachePurgeRequest, urlReq(s.ID, "https://x.test/a")).created(t)
	if yr.RequestID == first.RequestID || yr.Existing {
		t.Fatalf("connection Y got X's row: %+v", yr)
	}
}

func TestRail_DedupeRaceRetriesOnceThenFails(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://x.test")
	env := newRailEnv(t, f, nil)
	// A waiting row exists, so the insert conflicts; the first dedupe read
	// comes back empty (as if a person decided it in between). The rail
	// retries the insert exactly once; that conflicts again and the second
	// read finds the row: existing:true.
	f.seedRow(sqlc.AssistantCachePurgeRequest{TenantID: env.auth.TenantID, SiteID: s.ID,
		ProposedByGrantID: env.auth.GrantID, Scope: "all", State: "pending"})
	f.conflictReadMisses = 1
	r := env.call(ToolSiteCachePurgeRequest, allReq(s.ID)).created(t)
	if !r.Existing || f.insertCalls != 2 {
		t.Fatalf("existing=%v inserts=%d, want existing and 2 inserts", r.Existing, f.insertCalls)
	}
	// Two empty reads: -32603, and nothing written.
	f.insertCalls = 0
	f.conflictReadMisses = 2
	c := env.call(ToolSiteCachePurgeRequest, allReq(s.ID))
	c.wantCode(t, codeInternalError)
	if f.insertCalls != 2 {
		t.Fatalf("inserts = %d, want exactly 2 (one retry)", f.insertCalls)
	}
}

func TestRail_InLineExpiryIsAuditedAfterTheInsert(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://x.test")
	env := newRailEnv(t, f, nil)
	lapsed := f.seedRow(sqlc.AssistantCachePurgeRequest{TenantID: env.auth.TenantID, SiteID: s.ID,
		ProposedByGrantID: env.auth.GrantID, Scope: "all", State: "pending",
		ExpiresAt: time.Now().Add(-time.Minute)})
	f.ops = nil
	r := env.call(ToolSiteCachePurgeRequest, allReq(s.ID)).created(t)
	if r.Existing || r.RequestID == lapsed.ID.String() {
		t.Fatalf("the lapsed row was reused: %+v", r)
	}
	order := strings.Join(f.ops, ",")
	lock := strings.Index(order, "lock:"+grantRequestLockKey)
	exp := strings.Index(order, "expire")
	ins := strings.Index(order, "insert")
	aexp := strings.Index(order, "audit:"+audit.ActionAssistantRequestExpired)
	acall := strings.Index(order, "audit:"+audit.ActionMCPToolCalled)
	if !(lock >= 0 && lock < exp && exp < ins && ins < aexp && aexp < acall) {
		t.Fatalf("order is %s; want lock, expire, insert, expired audit, tool.called audit", order)
	}
	for _, row := range f.rowsFor(env.auth.GrantID) {
		if row.ID == lapsed.ID && row.State != "expired" {
			t.Fatalf("lapsed row state %q", row.State)
		}
	}
}

func TestRail_CapsCountTheWholeScope(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		f := newRailFake()
		a := f.addSite("https://a.test")
		sites := []uuid.UUID{a.ID}
		for i := 0; i < 10; i++ {
			sites = append(sites, f.addSite(fmt.Sprintf("https://p%d.test", i)).ID)
		}
		env := newRailEnv(t, f, nil)
		for _, id := range sites[1:] {
			f.seedRow(sqlc.AssistantCachePurgeRequest{TenantID: env.auth.TenantID, SiteID: id,
				ProposedByGrantID: env.auth.GrantID, Scope: "all", State: "pending"})
		}
		c := env.call(ToolSiteCachePurgeRequest, allReq(a.ID))
		c.wantCode(t, codeRateLimited)
		if c.data["limit_scope"] != limitScopePending {
			t.Fatalf("limit_scope %v", c.data["limit_scope"])
		}
		// A repeat of a waiting request is not a new one: existing:true.
		r := env.call(ToolSiteCachePurgeRequest, allReq(sites[1])).created(t)
		if !r.Existing {
			t.Fatal("a repeat at the cap was refused")
		}
		for _, p := range f.txPrincipals {
			if len(p.AllowedSiteIDs) != len(sites) {
				t.Fatalf("the creation transaction ran under %d sites, want the whole scope (%d)",
					len(p.AllowedSiteIDs), len(sites))
			}
		}
	})
	t.Run("daily", func(t *testing.T) {
		f := newRailFake()
		a := f.addSite("https://a.test")
		b := f.addSite("https://b.test")
		env := newRailEnv(t, f, nil)
		for i := 0; i < 30; i++ {
			f.seedRow(sqlc.AssistantCachePurgeRequest{TenantID: env.auth.TenantID, SiteID: b.ID,
				ProposedByGrantID: env.auth.GrantID, Scope: "all", State: "rejected"})
		}
		c := env.call(ToolSiteCachePurgeRequest, allReq(a.ID))
		c.wantCode(t, codeRateLimited)
		if c.data["limit_scope"] != limitScopeGrantDaily {
			t.Fatalf("limit_scope %v", c.data["limit_scope"])
		}
	})
	t.Run("site hourly", func(t *testing.T) {
		f := newRailFake()
		a := f.addSite("https://a.test")
		env := newRailEnv(t, f, nil)
		f.clears[a.ID] = 12
		c := env.call(ToolSiteCachePurgeRequest, allReq(a.ID))
		c.wantCode(t, codeRateLimited)
		if c.data["limit_scope"] != limitScopeSiteHourly {
			t.Fatalf("limit_scope %v", c.data["limit_scope"])
		}
		ev, _ := env.rec.last(audit.ActionMCPToolDenied)
		if ev.Metadata["refusal_reason"] != string(reasonSiteHourlyCap) {
			t.Fatalf("denial reason %v", ev.Metadata["refusal_reason"])
		}
	})
}

// TestRail_AuditFailureLeavesNoRow: the creation's audit rows live in its
// transaction, so a failed append leaves no request row.
func TestRail_AuditFailureLeavesNoRow(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://x.test")
	env := newRailEnv(t, f, nil)
	env.rec.failInTx = true
	env.call(ToolSiteCachePurgeRequest, allReq(s.ID)).wantCode(t, codeInternalError)
	if n := len(f.rowsFor(env.auth.GrantID)); n != 0 {
		t.Fatalf("%d rows survive a failed audit append", n)
	}
}

// TestRail_CreationRecordsOneToolCalledRow: the rail owns mcp.tool.called,
// and the transport does not add a second.
func TestRail_CreationRecordsOneToolCalledRow(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://x.test")
	env := newRailEnv(t, f, nil)
	r := env.call(ToolSiteCachePurgeRequest, allReq(s.ID)).created(t)
	ev := env.rec.only(t, audit.ActionMCPToolCalled)
	if ev.Metadata["request_id"] != r.RequestID || ev.Metadata["existing"] != false || ev.Metadata["scope"] != "all" {
		t.Fatalf("tool.called metadata %v", ev.Metadata)
	}
}

func TestRail_PerProcessLimitIsLogOnly(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://x.test")
	env := newRailEnv(t, f, nil)
	env.call(ToolSiteCachePurgeRequest, allReq(s.ID)).created(t)
	before := len(env.rec.actions())
	c := env.callKeepLimit(ToolSiteCachePurgeRequest, allReq(s.ID))
	c.wantCode(t, codeRateLimited)
	if c.data["limit_scope"] != limitScopeRequestSite {
		t.Fatalf("limit_scope %v", c.data["limit_scope"])
	}
	if after := len(env.rec.actions()); after != before {
		t.Fatalf("the per-process limit wrote %d audit rows, want none", after-before)
	}
}

// TestRail_EmptyScopeIsMinus32002: the transport's gate refuses first; the
// rail refuses again if reached.
func TestRail_EmptyScopeIsMinus32002(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://x.test")
	env := newRailEnv(t, f, []uuid.UUID{})
	env.call(ToolSiteCachePurgeRequest, allReq(s.ID)).wantCode(t, codeScopeEmpty)
}
