import { describe, it, expect } from "vitest";

import { isAgentPluginKey, isAgentPluginComponent } from "./agent-plugin";

// Mirrors apps/api/internal/agentplugin/agentplugin_test.go's TestIs case for
// case, so a future edit to either side that drifts from the other is caught
// on whichever side runs first.

describe("isAgentPluginKey", () => {
  const cases: Array<[string, boolean]> = [
    // The keys WordPress actually reports for the two distributions.
    ["wpmgr-agent/wpmgr-agent.php", true],
    ["fleet-agent-site-manager/fleet-agent-site-manager.php", true],
    // Bare directory, single file, stray leading slash, padding, case.
    ["wpmgr-agent", true],
    ["wpmgr-agent.php", true],
    ["/wpmgr-agent/wpmgr-agent.php", true],
    ["  wpmgr-agent/wpmgr-agent.php  ", true],
    ["WPMGR-Agent/WPMGR-Agent.PHP", true],
    ["fleet-agent-site-manager", true],
    // A renamed main file inside the agent's own directory still matches:
    // the directory is what identifies the plugin.
    ["wpmgr-agent/loader.php", true],
    // Everything else, including names that merely start or end with the
    // agent's slug, must not match.
    ["", false],
    ["akismet/akismet.php", false],
    ["wpmgr-agent-extras/wpmgr-agent-extras.php", false],
    ["my-wpmgr-agent/my-wpmgr-agent.php", false],
    ["agent/wpmgr-agent.php", false],
    ["twentytwentyfour", false],
  ];

  it.each(cases)("isAgentPluginKey(%j) === %s", (key, want) => {
    expect(isAgentPluginKey(key)).toBe(want);
  });
});

// The name branch is what survives a renamed install directory. A host
// uploader that derives the folder from the release zip produces a slug
// matching neither shipped distribution, so without this the agent stays
// selectable in the wizard.
describe("isAgentPluginComponent", () => {
  const cases: Array<[string, string | null | undefined, boolean]> = [
    // Slug alone still identifies it, name absent or unhelpful.
    ["wpmgr-agent/wpmgr-agent.php", undefined, true],
    ["wpmgr-agent/wpmgr-agent.php", "Site Agent", true],
    // Renamed directory: only the plugin-header name identifies it.
    ["wpmgr-agent-0.61.88/wpmgr-agent.php", "WPMgr Agent", true],
    ["totally-random-folder/x.php", "Fleet Agent Site Manager", true],
    ["renamed/x.php", "  wpmgr agent  ", true],
    // The inverse direction matters just as much: over-matching here would
    // silently withhold a customer's own plugin updates.
    ["wpmgr-agent-pro/wpmgr-agent-pro.php", "WPMgr Agent Pro", false],
    ["my-wpmgr-agent/my-wpmgr-agent.php", "My WPMgr Agent", false],
    ["fleet-agent-site-manager-addon/addon.php", "Fleet Agent Site Manager Pro", false],
    ["akismet/akismet.php", "Akismet", false],
    ["some-plugin/x.php", null, false],
    ["", "", false],
  ];

  it.each(cases)(
    "isAgentPluginComponent(%j, %j) === %s",
    (key, name, want) => {
      expect(isAgentPluginComponent(key, name)).toBe(want);
    },
  );
});
