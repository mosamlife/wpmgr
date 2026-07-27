// Mirrors the control plane's `internal/agentplugin.Is` (Go) so the update
// wizard can never offer the WPMgr agent's own plugin entry as an update
// target. A plugin update applied to the agent is an in-process
// self-overwrite (the code being replaced is the code performing the
// replacement), so the snapshot/rollback that guards every other update
// cannot be delivered if the swap goes wrong. The control plane ships agent
// upgrades over its own signed self-update channel instead.
//
// The control plane already strips the `available_update` advisory for the
// agent's own inventory entry at the source (sanitizeComponents); this is
// the second, belt-and-braces gate on the client so a stale cache entry or
// a hand-built payload can never surface it as selectable here either.

const AGENT_SLUGS: ReadonlySet<string> = new Set([
  // The self-hosted distribution (make agent-zip).
  "wpmgr-agent",
  // The public plugin-directory distribution (make agent-zip-wporg).
  "fleet-agent-site-manager",
]);

/**
 * True when `key` identifies the WPMgr agent's own plugin entry.
 *
 * `key` is a plugin inventory key in the form WordPress reports it: the
 * plugin file relative to the plugins directory (e.g.
 * "wpmgr-agent/wpmgr-agent.php"), which update items reuse verbatim as
 * their target slug. Matches the bare-directory and single-file forms too
 * (case-insensitive, tolerant of a stray leading slash or padding) so a
 * hand-built or replayed payload cannot dodge the check by dropping the
 * file segment.
 */
export function isAgentPluginKey(key: string): boolean {
  let dir = key.trim().toLowerCase();
  if (dir.startsWith("/")) dir = dir.slice(1);
  const slashIndex = dir.indexOf("/");
  if (slashIndex >= 0) dir = dir.slice(0, slashIndex);
  if (dir.endsWith(".php")) dir = dir.slice(0, -".php".length);
  return AGENT_SLUGS.has(dir);
}

// Plugin-header "Plugin Name" values, one per distribution. Mirrors
// `agentplugin.NameSelfHosted` / `NameDirectory` in Go. The header name
// survives a renamed install directory, which the slug does not: a host
// uploader that derives the folder from the release zip yields something
// like "wpmgr-agent-0.61.88/wpmgr-agent.php", whose directory segment
// matches neither shipped slug.
const AGENT_NAMES: ReadonlySet<string> = new Set([
  "wpmgr agent",
  "fleet agent site manager",
]);

/**
 * True when an inventory entry identifies the WPMgr agent, by either its
 * slug or its plugin-header name. Mirrors `agentplugin.IsComponent` in Go.
 *
 * The name is compared as a whole value, never as a prefix or substring, so
 * a genuinely different plugin whose name merely resembles the agent's
 * ("WPMgr Agent Pro", "My WPMgr Agent") is NOT matched. Over-matching here
 * would silently withhold a customer's own plugin updates, which is its own
 * defect rather than a safe default.
 */
export function isAgentPluginComponent(
  key: string,
  name?: string | null,
): boolean {
  if (isAgentPluginKey(key)) return true;
  if (!name) return false;
  return AGENT_NAMES.has(name.trim().toLowerCase());
}
