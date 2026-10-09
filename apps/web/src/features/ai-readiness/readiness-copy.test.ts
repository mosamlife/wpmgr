import { describe, expect, it } from "vitest";
import type { AiReadinessCheckId } from "@wpmgr/api";

import { FLOORS } from "./readiness-fixtures";
import {
  describeCheck,
  freshnessLine,
  groupHeading,
  iconFor,
  OTHER_CHECK_LABEL,
  OTHER_WARNING_COPY,
  statusLine,
  warningCopy,
  WARNING_COPY,
  builderHeaderRight,
  comingNote,
  notInstalledLine,
  type CheckInput,
  type CopyContext,
} from "./readiness-copy";

// The copy is the owner's acceptance text, so every assertion here is an
// explicit string, not a snapshot. The (check, state, reason) triples below are
// the ones apps/api/internal/aireadiness/evaluate.go emits; a triple that file
// cannot produce is not asserted as if the server sent it.

const OPERATOR: CopyContext = { floors: FLOORS, canOperate: true };
const VIEWER: CopyContext = { floors: FLOORS, canOperate: false };

function c(
  id: string,
  state: string,
  reason: string | null = null,
  observed: string | null = null,
): CheckInput {
  return { id, state, reason, observed };
}

function detail(check: CheckInput, ctx: CopyContext = OPERATOR): string {
  return describeCheck(check, ctx).detail;
}

describe("row labels come from the floors, not from literals", () => {
  it("states the floors the server sent", () => {
    const ctx: CopyContext = {
      floors: { wp: "8.0", agent: "1.2.3", facts_agent: "1.2.4", elementor: "5.0", bricks: "3.1" },
      canOperate: true,
    };
    expect(describeCheck(c("wp_version", "pass", null, "8.0"), ctx).label).toBe("WordPress 8.0 or later");
    expect(describeCheck(c("agent_version", "pass", null, "1.2.3"), ctx).label).toBe(
      "WPMgr plugin 1.2.3 or later",
    );
    expect(describeCheck(c("elementor_version", "pass", null, "5.0.1"), ctx).label).toBe(
      "Elementor 5.0 or later",
    );
    expect(describeCheck(c("bricks_version", "pass", null, "3.1"), ctx).label).toBe("Bricks 3.1 or later");
  });

  it("uses the design's fixed labels for the rows with no version in them", () => {
    expect(describeCheck(c("abilities_api", "pass"), OPERATOR).label).toBe("WordPress abilities");
    expect(describeCheck(c("content_editing", "pass"), OPERATOR).label).toBe("AI page creation");
    expect(describeCheck(c("elementor_mcp_switch", "pass"), OPERATOR).label).toBe("Elementor AI tools switch");
    expect(describeCheck(c("elementor_atomic", "pass"), OPERATOR).label).toBe("Elementor Atomic editor");
    expect(describeCheck(c("bricks_abilities", "pass"), OPERATOR).label).toBe("Bricks AI abilities");
  });
});

describe("WordPress and WPMgr group", () => {
  it("wp_version", () => {
    expect(detail(c("wp_version", "pass", null, "7.1.2"))).toBe("WordPress 7.1.2.");
    expect(detail(c("wp_version", "fail", null, "7.0.9"))).toBe(
      "WordPress 7.0.9. Builder tools need 7.1 or later. Update WordPress on this site.",
    );
    expect(detail(c("wp_version", "unknown", "not_reported"))).toBe(
      "WordPress has not reported its version yet.",
    );
  });

  it("abilities_api", () => {
    expect(detail(c("abilities_api", "pass"))).toBe("Available.");
    expect(detail(c("abilities_api", "fail"))).toBe("Not available. WordPress 6.9 or later includes it.");
    expect(detail(c("abilities_api", "unknown", "inventory_never_run"))).toBe("Not checked yet.");
    expect(detail(c("abilities_api", "unknown", "agent_too_old"))).toBe(
      "Update the WPMgr plugin to check this.",
    );
    expect(detail(c("abilities_api", "unknown", "not_reported"))).toBe(
      "WordPress has not reported its version yet.",
    );
  });

  it("agent_version", () => {
    expect(detail(c("agent_version", "pass", null, "0.61.159"))).toBe("Version 0.61.159.");
    expect(detail(c("agent_version", "fail", null, "0.61.100"))).toBe(
      "Version 0.61.100. Update the WPMgr plugin to 0.61.158 or later.",
    );
    expect(detail(c("agent_version", "unknown", "not_reported"))).toBe(
      "The WPMgr plugin has not reported its version yet.",
    );
  });

  it("content_editing says who can turn it on, by role", () => {
    expect(detail(c("content_editing", "pass"))).toBe("On.");
    expect(detail(c("content_editing", "fail"), OPERATOR)).toBe("Off. Turn it on in AI editing, below.");
    expect(detail(c("content_editing", "fail"), VIEWER)).toBe(
      "Off. An operator can turn it on in AI editing, below.",
    );
  });
});

describe("Elementor group", () => {
  it("elementor_version", () => {
    expect(detail(c("elementor_version", "pass", null, "4.3.4"))).toBe("Elementor 4.3.4, active.");
    expect(detail(c("elementor_version", "fail", "too_old", "4.2.9"))).toBe(
      "Elementor 4.2.9. Its AI tools need 4.3 or later. Update Elementor.",
    );
    expect(detail(c("elementor_version", "unknown", "not_reported"))).toBe(
      "Elementor has not reported its version yet.",
    );
  });

  it("elementor_mcp_switch", () => {
    expect(detail(c("elementor_mcp_switch", "pass"))).toBe("On.");
    expect(detail(c("elementor_mcp_switch", "fail"))).toBe(
      "Off. A site administrator turns it on in the WordPress admin, in Elementor's MCP settings.",
    );
    expect(detail(c("elementor_mcp_switch", "unknown", "inventory_never_run"))).toBe("Not checked yet.");
    expect(detail(c("elementor_mcp_switch", "unknown", "inventory_truncated"))).toBe(
      "Could not tell: this site lists more tools than WPMgr reads.",
    );
    expect(detail(c("elementor_mcp_switch", "not_applicable", "needs_abilities"))).toBe(
      "Needs WordPress abilities first.",
    );
    expect(detail(c("elementor_mcp_switch", "not_applicable", "needs_elementor"))).toBe(
      "Needs Elementor 4.3 or later first.",
    );
    expect(detail(c("elementor_mcp_switch", "unknown", "needs_elementor"))).toBe(
      "Needs Elementor 4.3 or later first.",
    );
  });

  it("elementor_atomic", () => {
    expect(detail(c("elementor_atomic", "pass"))).toBe("On.");
    expect(detail(c("elementor_atomic", "fail"))).toBe(
      "Off. Without it the AI can only create a blank Elementor page. A site administrator turns it on in the WordPress admin: Elementor → Settings → Atomic Editor.",
    );
    expect(detail(c("elementor_atomic", "unknown", "agent_too_old_for_fact"))).toBe(
      "Update the WPMgr plugin to 0.61.159 or later to check this.",
    );
    expect(detail(c("elementor_atomic", "unknown", "not_reported"))).toBe("Not reported yet.");
    expect(detail(c("elementor_atomic", "unknown", "needs_elementor"))).toBe(
      "Needs Elementor 4.3 or later first.",
    );
    expect(detail(c("elementor_atomic", "not_applicable", "needs_elementor"))).toBe(
      "Needs Elementor 4.3 or later first.",
    );
  });
});

describe("Bricks group", () => {
  it("bricks_version", () => {
    expect(detail(c("bricks_version", "pass", null, "2.4.1"))).toBe("Bricks 2.4.1, active theme.");
    expect(detail(c("bricks_version", "fail", "too_old", "2.3"))).toBe(
      "Bricks 2.3. Its AI abilities need 2.4 or later. Update Bricks.",
    );
    expect(detail(c("bricks_version", "unknown", "agent_too_old_for_fact", "2.4.1"))).toBe(
      "Update the WPMgr plugin to 0.61.159 or later to check this.",
    );
    expect(detail(c("bricks_version", "unknown", "not_reported", "2.4.1"))).toBe("Not reported yet.");
  });

  it("bricks_abilities", () => {
    // A pass or a fail is derived from the tool list and unconfirmed: see the
    // "derived and unconfirmed" block below. Only the rows that claim nothing
    // about On or Off keep their own copy.
    expect(detail(c("bricks_abilities", "unknown", "inventory_never_run"))).toBe("Not checked yet.");
    expect(detail(c("bricks_abilities", "unknown", "inventory_truncated"))).toBe(
      "Could not tell: this site lists more tools than WPMgr reads.",
    );
    expect(detail(c("bricks_abilities", "not_applicable", "needs_abilities"))).toBe(
      "Needs WordPress abilities first.",
    );
    expect(detail(c("bricks_abilities", "not_applicable", "needs_bricks"))).toBe(
      "Needs Bricks 2.4 or later first.",
    );
    expect(detail(c("bricks_abilities", "unknown", "needs_bricks"))).toBe(
      "Needs Bricks 2.4 or later first.",
    );
  });
});

describe("a builder that is installed but not active", () => {
  const BUILDERS = [
    { id: "elementor_version", observed: "4.3.4", label: "Elementor 4.3 or later" },
    { id: "bricks_version", observed: "2.4.1", label: "Bricks 2.4 or later" },
  ] as const;

  // The control plane sends the version row of an installed, inactive builder
  // as state not_applicable with reason inactive, and `observed` still carries
  // the installed version (packages/openapi/openapi.yaml, AIReadinessCheck;
  // apps/api/internal/aireadiness/evaluate.go, inactive()).
  it.each(BUILDERS)("$id reads grey 'Installed, not active.'", ({ id, observed, label }) => {
    const row = describeCheck(c(id, "not_applicable", "inactive", observed), OPERATOR);
    expect(row.tone).toBe("neutral");
    expect(row.iconLabel).toBe("Not active");
    expect(row.detail).toBe("Installed, not active.");
    // The row still says what it is about.
    expect(row.label).toBe(label);
  });

  // An earlier control plane sent the same cause as a fail. Rolling the page
  // out ahead of the API must not paint it red.
  it.each(BUILDERS)("$id reads the same when an older control plane still sends a fail", ({ id, observed }) => {
    const row = describeCheck(c(id, "fail", "inactive", observed), OPERATOR);
    expect(row.tone).toBe("neutral");
    expect(row.iconLabel).toBe("Not active");
    expect(row.detail).toBe("Installed, not active.");
  });

  it.each(BUILDERS)("$id still reads green when it passes, and red when the version is too old", ({ id }) => {
    const pass = describeCheck(c(id, "pass", null, "9.9"), OPERATOR);
    expect(pass.tone).toBe("pass");
    expect(pass.iconLabel).toBe("Passed");

    const tooOld = describeCheck(c(id, "fail", "too_old", "1.0"), OPERATOR);
    expect(tooOld.tone).toBe("fail");
    expect(tooOld.iconLabel).toBe("Needs fixing");
  });

  it("does not read the reason `inactive` as a builder row on any other check", () => {
    const row = describeCheck(c("wp_version", "fail", "inactive", "7.0"), OPERATOR);
    expect(row.tone).toBe("fail");
    expect(row.detail).not.toBe("Installed, not active.");
  });

  it("makes the rows that wait on it say it has to be active, not a newer version", () => {
    const inactive: CopyContext = { ...OPERATOR, builderInactive: true };
    expect(detail(c("elementor_mcp_switch", "not_applicable", "needs_elementor"), inactive)).toBe(
      "Needs Elementor to be active first.",
    );
    expect(detail(c("elementor_atomic", "not_applicable", "needs_elementor"), inactive)).toBe(
      "Needs Elementor to be active first.",
    );
    expect(detail(c("bricks_abilities", "not_applicable", "needs_bricks"), inactive)).toBe(
      "Needs Bricks to be active first.",
    );
    // A row waiting on something else keeps its own words.
    expect(detail(c("elementor_mcp_switch", "not_applicable", "needs_abilities"), inactive)).toBe(
      "Needs WordPress abilities first.",
    );
  });

  it("still asks for the newer version when the builder is active but too old", () => {
    expect(detail(c("elementor_mcp_switch", "not_applicable", "needs_elementor"), OPERATOR)).toBe(
      "Needs Elementor 4.3 or later first.",
    );
  });
});

describe("a row that is derived and unconfirmed", () => {
  it("reads grey 'Derived, unconfirmed.' with a short note, for a pass", () => {
    const row = describeCheck(c("bricks_abilities", "pass"), OPERATOR);
    expect(row.tone).toBe("neutral");
    expect(row.iconLabel).toBe("Unconfirmed");
    expect(row.label).toBe("Bricks AI abilities");
    expect(row.detail).toBe(
      "Derived, unconfirmed. Bricks tools are listed on this site. Not yet checked on a licensed Bricks install.",
    );
  });

  it("reads the same grey for a fail, never a red cross", () => {
    const row = describeCheck(c("bricks_abilities", "fail"), OPERATOR);
    expect(row.tone).toBe("neutral");
    expect(row.iconLabel).toBe("Unconfirmed");
    expect(row.detail).toBe(
      "Derived, unconfirmed. No Bricks tools are listed on this site. Not yet checked on a licensed Bricks install.",
    );
  });

  it("never says On or Off", () => {
    for (const state of ["pass", "fail"]) {
      expect(detail(c("bricks_abilities", state))).not.toMatch(/\bOn\b|\bOff\b/);
    }
  });

  it("is the same for an operator and a viewer: there is nothing to turn on from here", () => {
    expect(detail(c("bricks_abilities", "fail"), VIEWER)).toBe(detail(c("bricks_abilities", "fail"), OPERATOR));
  });

  it("leaves rows that claim nothing about On or Off with their own copy and icon", () => {
    const never = describeCheck(c("bricks_abilities", "unknown", "inventory_never_run"), OPERATOR);
    expect(never.detail).toBe("Not checked yet.");
    expect(never.iconLabel).toBe("Not checked");
    const waits = describeCheck(c("bricks_abilities", "not_applicable", "needs_bricks"), OPERATOR);
    expect(waits.detail).toBe("Needs Bricks 2.4 or later first.");
    expect(waits.iconLabel).toBe("Not applicable");
  });

  it("applies to the Bricks row only: Elementor's switch still reads On or Off", () => {
    const on = describeCheck(c("elementor_mcp_switch", "pass"), OPERATOR);
    expect(on.tone).toBe("pass");
    expect(on.detail).toBe("On.");
    const off = describeCheck(c("elementor_mcp_switch", "fail"), OPERATOR);
    expect(off.tone).toBe("fail");
  });
});

describe("icons: only a state the server named `fail` is red", () => {
  it("maps each known state to its tone and accessible name", () => {
    expect(iconFor("pass")).toEqual({ tone: "pass", label: "Passed" });
    expect(iconFor("fail")).toEqual({ tone: "fail", label: "Needs fixing" });
    expect(iconFor("unknown")).toEqual({ tone: "neutral", label: "Not checked" });
    expect(iconFor("not_applicable")).toEqual({ tone: "neutral", label: "Not applicable" });
  });

  it("renders a state it has never heard of as grey, never red", () => {
    expect(iconFor("exploded")).toEqual({ tone: "neutral", label: "Not checked" });
    expect(iconFor("")).toEqual({ tone: "neutral", label: "Not checked" });
  });

  it("renders a check id it has never heard of as grey even when the server says fail", () => {
    const row = describeCheck(c("elementor_role_access", "fail"), OPERATOR);
    expect(row.tone).toBe("neutral");
    expect(row.iconLabel).toBe("Not checked");
    expect(row.label).toBe(OTHER_CHECK_LABEL);
  });

  it("keeps the state's icon for an unrecognised reason on a known check and falls back to neutral copy", () => {
    const unknownRow = describeCheck(c("elementor_atomic", "unknown", "some_new_reason"), OPERATOR);
    expect(unknownRow.tone).toBe("neutral");
    expect(unknownRow.detail).toBe("Not checked yet.");

    const failRow = describeCheck(c("elementor_version", "fail", "too_new", "9.0"), OPERATOR);
    expect(failRow.tone).toBe("fail");
    expect(failRow.detail).toBe("Needs fixing.");
  });

  it("covers every check id in the contract with a real label", () => {
    // `satisfies` makes this list fail typecheck the day the contract adds an id.
    const ids = {
      wp_version: true,
      abilities_api: true,
      agent_version: true,
      content_editing: true,
      elementor_version: true,
      elementor_mcp_switch: true,
      elementor_atomic: true,
      bricks_version: true,
      bricks_abilities: true,
    } satisfies Record<AiReadinessCheckId, true>;
    for (const id of Object.keys(ids)) {
      expect(describeCheck(c(id, "pass"), OPERATOR).label).not.toBe(OTHER_CHECK_LABEL);
    }
  });
});

describe("status line", () => {
  it("says ready", () => {
    expect(statusLine("ready", 0)).toEqual({
      tone: "ready",
      text: "Ready. Everything the AI needs on this site is in place.",
    });
  });

  it("counts what needs fixing, singular and plural", () => {
    expect(statusLine("needs_attention", 1)).toEqual({
      tone: "attention",
      text: "1 thing to fix before the AI can work here.",
    });
    expect(statusLine("needs_attention", 2)).toEqual({
      tone: "attention",
      text: "2 things to fix before the AI can work here.",
    });
  });

  it("treats incomplete, and a status it does not know, as not yet checked", () => {
    expect(statusLine("incomplete", 0)).toEqual({ tone: "neutral", text: "Some checks have not run yet." });
    expect(statusLine("something_new", 3)).toEqual({ tone: "neutral", text: "Some checks have not run yet." });
  });
});

describe("freshness line", () => {
  const now = Date.parse("2026-10-09T12:00:00Z");
  const fiveMin = "2026-10-09T11:55:00Z";
  const twoHours = "2026-10-09T10:00:00Z";

  it("names both times", () => {
    expect(freshnessLine(fiveMin, twoHours, now)).toBe("Site details from 5m ago. Tool list from 2h ago.");
  });

  it("says what has not happened yet", () => {
    expect(freshnessLine(null, twoHours, now)).toBe("Site details not reported yet. Tool list from 2h ago.");
    expect(freshnessLine(fiveMin, null, now)).toBe("Site details from 5m ago. Tool list not read yet.");
    expect(freshnessLine(null, null, now)).toBe("Site details not reported yet. Tool list not read yet.");
  });

  it("does not print a time it cannot parse", () => {
    expect(freshnessLine("not a date", twoHours, now)).toBe(
      "Site details not reported yet. Tool list from 2h ago.",
    );
  });
});

describe("warnings", () => {
  it("states the design's two notices word for word", () => {
    expect(warningCopy("mcp_adapter_plugin_active")).toBe(
      "The WordPress MCP Adapter plugin is active on this site. It lets any logged-in user of the site connect an AI tool directly, without WPMgr's approvals, undo or audit. WPMgr does not need it. If nobody uses it, consider deactivating it.",
    );
    expect(warningCopy("elementor_mcp_endpoint_open")).toBe(
      "Elementor's AI tools switch also opens Elementor's own AI connection point on this site. Any logged-in user can connect an AI tool to it, without WPMgr's approvals, undo or audit.",
    );
  });

  it("never suggests installing the adapter", () => {
    for (const text of [...Object.values(WARNING_COPY), OTHER_WARNING_COPY]) {
      expect(text.toLowerCase()).not.toContain("install");
    }
  });

  it("does not claim that switching Elementor's switch off closes every connection point", () => {
    expect(WARNING_COPY.elementor_mcp_endpoint_open.toLowerCase()).not.toMatch(/turn(ing)? (it )?off|switch(ing)? (it )?off|closes/);
  });

  it("shows a neutral notice for a warning code it does not know", () => {
    expect(warningCopy("brand_new_warning")).toBe(OTHER_WARNING_COPY);
  });
});

describe("group headings", () => {
  it("titles the three groups", () => {
    expect(groupHeading("base")).toEqual({ title: "WordPress and WPMgr", builder: null });
    expect(groupHeading("elementor")).toEqual({ title: "Elementor", builder: "Elementor" });
    expect(groupHeading("bricks")).toEqual({ title: "Bricks", builder: "Bricks" });
  });

  it("gives a group it does not know a neutral title and no builder sentences", () => {
    expect(groupHeading("divi")).toEqual({ title: "Other checks", builder: null });
  });

  it("writes the right side of a builder header", () => {
    expect(builderHeaderRight({ installed: true, version: "4.3.4" })).toBe("Version 4.3.4");
    expect(builderHeaderRight({ installed: false, version: null })).toBe("Not installed");
    expect(builderHeaderRight({ installed: true, version: null })).toBe("Installed");
  });

  it("writes the not-installed and coming sentences with the builder's name", () => {
    expect(notInstalledLine("Elementor")).toBe("Elementor is not installed on this site. Nothing to check.");
    expect(comingNote("Bricks")).toBe(
      "WPMgr cannot build Bricks pages yet. These checks show whether this site will be ready.",
    );
  });
});
