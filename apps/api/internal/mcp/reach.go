package mcp

import (
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// THE HOSTING-CACHE REACH TABLE.
//
// One row per hosting-cache integration the WPMgr agent ships. It is the Go
// mirror of each integration's REACH and REACH_NOTE constants in
// apps/agent/includes/integrations, and it is the ONE source for every
// sentence that says which hosting caches an AI cache clear touches: the tool
// description (cachePurgeRequestDescription), the consent and wizard
// disclosure, and the request card. No such sentence is typed anywhere else.
//
// Reach is one of two values:
//
//   - reachShared (the default): the host's purge may reach beyond this one
//     site, or nobody has confirmed that it does not. An AI-requested clear
//     SKIPS it.
//   - reachInstall: WPMgr has confirmed, with a dated note, that the host's
//     purge reaches only this WordPress install. An AI-requested clear asks it
//     to clear.
//
// AT PRESENT EVERY ROW IS SHARED, so every hosting cache is skipped. Moving a
// row to reachInstall is its own change, in the agent and here together, and
// TestReachTable_InstallRowsCarryADatedNote refuses one without a note of the
// form "Verified YYYY-MM-DD: ...". TestReachTable_MatchesTheAgent keeps the
// slug set and each row's reach equal to the agent's.
// ---------------------------------------------------------------------------

type reach string

const (
	reachShared  reach = "shared"
	reachInstall reach = "install"
)

// urlClear is what a confirmed ('install') integration does for a one-page
// clear. It is recorded now so the rendered sentence is right the day a row is
// confirmed, and it has no effect while every row is shared.
type urlClear string

const (
	urlClearPerURL    urlClear = "per_url"    // clears that page
	urlClearWholeSite urlClear = "whole_site" // has no per-page clear; clears this site's whole cache there
	urlClearExactURL  urlClear = "exact_url"  // clears that exact address only
	urlClearNone      urlClear = "none"       // skipped even if confirmed
)

// hostingCacheReach is one row of the table.
type hostingCacheReach struct {
	// Slug is the agent's Integration::SLUG. It is also the closed set of
	// values the request status may report as cleared or skipped: the agent's
	// value is matched against it and our own constant is emitted.
	Slug string
	// Name is how operator copy names the host.
	Name     string
	Reach    reach
	Note     string
	URLClear urlClear
}

// hostingCacheReachTable returns the table, sorted by slug, as a fresh slice.
func hostingCacheReachTable() []hostingCacheReach {
	rows := []hostingCacheReach{
		{Slug: "cloudflare", Name: "Cloudflare", Reach: reachShared, URLClear: urlClearExactURL},
		{Slug: "cloudpanel", Name: "CloudPanel", Reach: reachShared, URLClear: urlClearNone},
		{Slug: "cloudways", Name: "Cloudways", Reach: reachShared, URLClear: urlClearNone},
		{Slug: "gridpane", Name: "GridPane", Reach: reachShared, URLClear: urlClearWholeSite},
		{Slug: "kinsta", Name: "Kinsta", Reach: reachShared, URLClear: urlClearPerURL},
		{Slug: "rocketnet", Name: "Rocket.net", Reach: reachShared, URLClear: urlClearNone},
		{Slug: "runcloud", Name: "RunCloud", Reach: reachShared, URLClear: urlClearPerURL},
		{Slug: "siteground", Name: "SiteGround", Reach: reachShared, URLClear: urlClearPerURL},
		{Slug: "spinupwp", Name: "SpinupWP", Reach: reachShared, URLClear: urlClearPerURL},
		{Slug: "varnish", Name: "Varnish", Reach: reachShared, URLClear: urlClearNone},
		{Slug: "wpcloud", Name: "WP Cloud", Reach: reachShared, URLClear: urlClearWholeSite},
		{Slug: "wpengine", Name: "WP Engine", Reach: reachShared, URLClear: urlClearWholeSite},
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Slug < rows[j].Slug })
	return rows
}

// HostingCacheSlugs is the closed slug set, sorted.
func HostingCacheSlugs() []string {
	rows := hostingCacheReachTable()
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Slug)
	}
	return out
}

// hostingCacheSentence renders what an AI-requested clear does to hosting
// caches, from the table. It never names a host that is not confirmed.
func hostingCacheSentence(rows []hostingCacheReach) string {
	var confirmed, wholeSite []string
	for _, r := range rows {
		if r.Reach != reachInstall {
			continue
		}
		confirmed = append(confirmed, r.Name)
		if r.URLClear == urlClearWholeSite {
			wholeSite = append(wholeSite, r.Name)
		}
	}
	if len(confirmed) == 0 {
		return "WPMgr's own hosting and CDN cache integrations are cleared only where WPMgr has " +
			"confirmed they clear only this site; at present none is confirmed, so all of them " +
			"are skipped and visitors may still get cached copies from the host. This limit " +
			"covers those integrations only, not other plugins' hooks on the purge actions."
	}
	out := "WPMgr's own hosting and CDN cache integrations are cleared only where WPMgr has " +
		"confirmed they clear only this site, which at present is " + joinNames(confirmed) +
		". Its other integrations are skipped, so visitors may still get cached copies from " +
		"those hosts. This limit covers those integrations only, not other plugins' hooks on " +
		"the purge actions."
	if len(wholeSite) > 0 {
		out += " On " + joinNames(wholeSite) + ", a `url` clear clears this site's whole cache " +
			"there, because that host has no per-page clear."
	}
	return out
}

func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}
