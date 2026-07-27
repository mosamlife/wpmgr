// Package agentplugin pins the identity of the WordPress plugin this control
// plane ships (the agent) so every layer recognizes the agent's own entry in a
// site's plugin inventory from ONE place.
//
// The agent must never be offered as an actionable update target. A plugin
// update applied to the agent is an in-process self-overwrite: the code being
// replaced is the code performing the replacement, so the snapshot/rollback
// that guards every other update cannot be delivered if the swap goes wrong.
// The control plane ships agent upgrades over its own signed self-update
// channel instead (ADR-042, GET /agent/v1/update/manifest), which the agent
// verifies before WP_Upgrader touches any file.
//
// The agent still appears in the inventory with its installed version; only
// the actionable "available_update" advisory is suppressed.
package agentplugin

import "strings"

// Distribution slugs the agent is installed under. WordPress derives a
// plugin's directory (and therefore its inventory key) from the archive folder
// name, so the two build targets in the Makefile produce two known slugs.
//
// A slug is only ever a HINT, never the whole identity: the folder name is
// chosen by whoever unpacked the archive. A release asset unzipped verbatim
// ("wpmgr-agent-0.61.88/"), a host control-panel uploader that appends a
// version, or an operator renaming the directory all produce a key no fixed
// list can predict. See DistributionOf for the identity that survives that.
const (
	// SlugSelfHosted is the self-hosted distribution (make agent-zip), which is
	// also the slug the ADR-042 release manifest is pinned to.
	SlugSelfHosted = "wpmgr-agent"

	// SlugDirectory is the public plugin-directory distribution
	// (make agent-zip-wporg), which stages the plugin under its own folder name
	// and renames the main PHP file to match.
	SlugDirectory = "fleet-agent-site-manager"
)

// Plugin-header "Plugin Name" values, one per distribution. WordPress reads
// these out of the main PHP file's header block, so they travel INSIDE the
// archive and survive any rename of the directory the plugin was unpacked
// into. The agent reports the header verbatim as each inventory entry's name
// (get_plugins() is called with translation off, so these are never localized).
//
// The wp.org build rewrites the header at stage time (see the agent-zip-wporg
// target's "Plugin Name:" sed), which is what makes NameDirectory a reliable
// marker of that build rather than a cosmetic difference.
const (
	// NameSelfHosted is the self-hosted build's plugin header name.
	NameSelfHosted = "WPMgr Agent"

	// NameDirectory is the public plugin-directory build's plugin header name.
	NameDirectory = "Fleet Agent Site Manager"
)

// Distribution names which build of the agent a site has installed. The two
// builds differ in a way the control plane must act on: the plugin-directory
// build ships with the self-updater source file excluded and WPMGR_WPORG_BUILD
// defined, so it can never consume the ADR-042 release channel.
type Distribution string

const (
	// DistributionNone means the inventory entry is not the agent's plugin at
	// all. It is the zero value, so an unpopulated Distribution never claims a
	// build.
	DistributionNone Distribution = ""

	// DistributionSelfHosted is the self-hosted build (make agent-zip). It
	// carries the signed self-updater and consumes the ADR-042 release channel.
	DistributionSelfHosted Distribution = "self_hosted"

	// DistributionDirectory is the public plugin-directory build
	// (make agent-zip-wporg). The Makefile physically excludes
	// includes/support/class-update-checker.php from the staged tree AND
	// injects WPMGR_WPORG_BUILD, so this build cannot self-update: it is
	// upgraded by the plugin directory, not by us.
	DistributionDirectory Distribution = "directory"
)

// DistributionOf identifies the agent from one plugin-inventory entry and
// reports which build it is, or DistributionNone when the entry is some other
// plugin.
//
// key is a plugin inventory key in the form WordPress reports it: the plugin
// file relative to the plugins directory ("wpmgr-agent/wpmgr-agent.php"), which
// update items reuse verbatim as their target slug. The bare directory form
// ("wpmgr-agent") and the single-file form ("wpmgr-agent.php") match too, so a
// hand-built or replayed payload cannot dodge the check by dropping the file
// segment. Matching is case-insensitive because a case-insensitive filesystem
// can hand WordPress a different case than the archive carried.
//
// name is the entry's plugin-header name (Component.Name). It is checked FIRST
// and wins over the slug, because the header travels inside the archive while
// the directory name does not: it is the only signal that still identifies an
// agent installed under a folder name we did not choose. Callers that genuinely
// have no name (an update task row carries only a slug) pass "" and fall back
// to the slug forms.
//
// Both halves match the WHOLE trimmed value and never a prefix, suffix, or
// substring, so a neighbouring plugin is never mistaken for the agent:
// "wpmgr-agent-pro", "my-wpmgr-agent" and "fleet-agent-site-manager-addon" are
// all DistributionNone, as are their header-name equivalents.
//
// Themes never carry these slugs or names, so callers may apply it to any
// component.
func DistributionOf(key, name string) Distribution {
	switch n := strings.TrimSpace(name); {
	case strings.EqualFold(n, NameSelfHosted):
		return DistributionSelfHosted
	case strings.EqualFold(n, NameDirectory):
		return DistributionDirectory
	}
	switch pluginDirectory(key) {
	case SlugSelfHosted:
		return DistributionSelfHosted
	case SlugDirectory:
		return DistributionDirectory
	}
	return DistributionNone
}

// IsComponent reports whether a plugin-inventory entry is the agent's own
// plugin. It is the predicate every read and write path over the inventory
// should use, because it can see the entry's plugin-header name.
func IsComponent(key, name string) bool {
	return DistributionOf(key, name) != DistributionNone
}

// Is reports whether key alone identifies the agent's own plugin.
//
// It is the slug-only form, for callers that hold a bare target slug and no
// inventory entry to go with it (update-item validation, an already-persisted
// task row). It cannot recognize an agent installed under an unexpected folder
// name; prefer IsComponent wherever the plugin-header name is at hand.
func Is(key string) bool {
	return DistributionOf(key, "") != DistributionNone
}

// pluginDirectory reduces an inventory key to its lowercased plugin directory:
// leading slash stripped, first path segment only, a trailing ".php" removed so
// the single-file form collapses onto the same value.
func pluginDirectory(key string) string {
	dir := strings.ToLower(strings.TrimSpace(key))
	dir = strings.TrimPrefix(dir, "/")
	if i := strings.IndexByte(dir, '/'); i >= 0 {
		dir = dir[:i]
	}
	return strings.TrimSuffix(dir, ".php")
}
