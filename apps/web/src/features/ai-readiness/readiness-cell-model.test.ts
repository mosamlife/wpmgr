import { describe, expect, it } from "vitest";
import type { FleetAiReadinessSite } from "@wpmgr/api";

import { cellFor, fleetCheckLabel, fleetWarningLine, type AiReadinessRollup } from "./readiness-cell-model";
import { fleetSite } from "./readiness-fixtures";

// The fleet rollup's shape is FleetAiReadiness in the generated client:
// { site_id, status, fix_count, failing: [check ids], warnings: [codes] }
// (apps/api/internal/aireadiness/dto.go fleetSiteDTO).

const A = "aaaaaaaa-0000-0000-0000-000000000001";
const B = "bbbbbbbb-0000-0000-0000-000000000002";

function ready(...sites: FleetAiReadinessSite[]): AiReadinessRollup {
  return { state: "ready", bySite: new Map(sites.map((s) => [s.site_id, s])) };
}

describe("cellFor", () => {
  it("shows Ready for a ready site", () => {
    const cell = cellFor(ready(fleetSite({ site_id: A })), A);
    expect(cell).toMatchObject({ kind: "result", tone: "ready", label: "Ready", hasWarning: false });
  });

  it("shows 'N to fix' and lists what to fix, one per line", () => {
    const cell = cellFor(
      ready(
        fleetSite({
          site_id: A,
          status: "needs_attention",
          fix_count: 2,
          failing: ["wp_version", "content_editing"],
        }),
      ),
      A,
    );
    expect(cell).toMatchObject({ kind: "result", tone: "attention", label: "2 to fix" });
    if (cell.kind !== "result") throw new Error("expected a result");
    expect(cell.lines).toEqual(["WordPress version", "AI page creation"]);
  });

  it("shows Not checked, in grey, for an incomplete site", () => {
    const cell = cellFor(ready(fleetSite({ site_id: A, status: "incomplete" })), A);
    expect(cell).toMatchObject({ kind: "result", tone: "neutral", label: "Not checked" });
  });

  it("shows Not checked, in grey, for a status it does not know", () => {
    const cell = cellFor(
      ready(fleetSite({ site_id: A, status: "from_the_future" as FleetAiReadinessSite["status"], fix_count: 4 })),
      A,
    );
    expect(cell).toMatchObject({ kind: "result", tone: "neutral", label: "Not checked" });
  });

  it("adds the warning line and flag without changing the label", () => {
    const cell = cellFor(
      ready(fleetSite({ site_id: A, warnings: ["mcp_adapter_plugin_active", "elementor_mcp_endpoint_open"] })),
      A,
    );
    expect(cell).toMatchObject({ kind: "result", tone: "ready", label: "Ready", hasWarning: true });
    if (cell.kind !== "result") throw new Error("expected a result");
    expect(cell.lines).toContain("Open AI connection point: MCP Adapter plugin");
    expect(cell.lines).toContain("Open AI connection point: Elementor switch");
  });

  it("reports loading while the rollup is on its way", () => {
    expect(cellFor({ state: "loading" }, A)).toEqual({ kind: "loading" });
  });

  it("reports unavailable, never an error, when the rollup failed or was refused", () => {
    expect(cellFor({ state: "unavailable" }, A)).toEqual({ kind: "unavailable" });
    expect(cellFor(undefined, A)).toEqual({ kind: "unavailable" });
  });

  it("reports unavailable for a site the rollup does not list", () => {
    expect(cellFor(ready(fleetSite({ site_id: A })), B)).toEqual({ kind: "unavailable" });
  });
});

describe("labels in the hover text", () => {
  it("names every check id the contract has, without a version number", () => {
    for (const id of [
      "wp_version",
      "abilities_api",
      "agent_version",
      "content_editing",
      "elementor_version",
      "elementor_mcp_switch",
      "elementor_atomic",
      "bricks_version",
      "bricks_abilities",
    ]) {
      const label = fleetCheckLabel(id);
      expect(label).not.toBe("Another check");
      expect(label).not.toMatch(/\d/);
    }
  });

  it("calls a check it does not know 'Another check'", () => {
    expect(fleetCheckLabel("elementor_role_access")).toBe("Another check");
  });

  it("describes a warning it does not know without naming one", () => {
    expect(fleetWarningLine("a_new_warning")).toBe("Open AI connection point: see this site's AI readiness");
  });
});
