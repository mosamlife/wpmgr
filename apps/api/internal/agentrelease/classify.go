package agentrelease

import (
	"regexp"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// Status is the per-site fleet-rollup classification (GET
// /api/v1/fleet/agents).
type Status string

const (
	// StatusCurrent: the site's reported agent_version is well-formed and >=
	// the currently published version.
	StatusCurrent Status = "current"
	// StatusOutdated: the site's reported agent_version is well-formed and
	// strictly older than the currently published version.
	StatusOutdated Status = "outdated"
	// StatusUnknown: either the site's agent_version or the currently
	// published version is empty or not well-formed enough to compare
	// safely, including a site that has never reported a version and a
	// published-version manifest that currently cannot be read.
	StatusUnknown Status = "unknown"
	// StatusIneligible: the site cannot self-update at all, so comparing its
	// version against the published one says nothing actionable. Today that
	// means the public plugin-directory build: the Makefile's agent-zip-wporg
	// target excludes includes/support/class-update-checker.php from the
	// staged tree and injects WPMGR_WPORG_BUILD, so the build has no
	// self-updater to run and is upgraded by the plugin directory instead.
	// Reporting such a site "outdated" would be a permanent false alarm
	// against a release channel it can never consume.
	StatusIneligible Status = "ineligible"
)

// versionPattern accepts the plain dotted-numeric shape WPMgr's own agent
// versions use (e.g. "0.61.95"): 2-4 numeric segments with an optional
// pre-release/build suffix. Anything else (empty, garbage, a lone word) is
// treated as not well-formed and classified StatusUnknown rather than risking
// a wrong ordering; this is deliberately stricter than wpversion.Compare's
// own tolerant tokenizer, which is designed to order WordPress core/plugin
// version strings it does not control, not to validate them.
var versionPattern = regexp.MustCompile(`^\d+(\.\d+){1,3}([-+][0-9A-Za-z.]+)?$`)

// isWellFormedVersion reports whether v looks like a genuine dotted-numeric
// version string this package is safe to order with wpversion.Compare.
func isWellFormedVersion(v string) bool {
	return versionPattern.MatchString(strings.TrimSpace(v))
}

// WellFormed reports whether v is a version string this package is willing to
// order. Exported for callers that must distinguish "this particular version is
// missing or garbage" from "these two versions compare thus": Classify collapses
// both unreadable sides into a single StatusUnknown, which cannot say WHICH one
// was unreadable. The agent self-update channel needs that distinction, because
// "the run recorded no target" and "the site reported no version" are different
// facts an operator has to act on differently.
func WellFormed(v string) bool { return isWellFormedVersion(v) }

// Classify compares a site's reported agent_version against the currently
// published latestVersion and returns the fleet-rollup status. Either side
// being empty or not well-formed (isWellFormedVersion) always yields
// StatusUnknown, never a false StatusOutdated.
//
// dist is the build of the agent the site's own plugin inventory identifies
// (agentplugin.DistributionOf, projected by ListSitesAgentVersions). The
// plugin-directory build is settled before any version comparison: it has no
// self-updater at all, so it is StatusIneligible whether or not the two
// versions are comparable. Every other value, including the zero
// DistributionNone for a site whose inventory identifies no agent, falls
// through to the version comparison unchanged.
func Classify(siteVersion, latestVersion string, dist agentplugin.Distribution) Status {
	if dist == agentplugin.DistributionDirectory {
		return StatusIneligible
	}
	if !isWellFormedVersion(siteVersion) || !isWellFormedVersion(latestVersion) {
		return StatusUnknown
	}
	if wpversion.Compare(siteVersion, latestVersion) < 0 {
		return StatusOutdated
	}
	return StatusCurrent
}
