import type {
  AiReadinessCheckId,
  AiReadinessWarningCode,
  FleetAiReadinessSite,
} from "@wpmgr/api";

import { OTHER_CHECK_LABEL, type StatusTone } from "./readiness-copy";

// The Sites list "AI" column and the grid chip, as plain data.
//
// The fleet rollup (GET /api/v1/fleet/ai-readiness) carries a status, a fix
// count, the ids of the failing rows and the warning codes. It carries no
// floors and no per-row detail, so the labels here are the floor-free short
// forms; the full row copy lives on the site's Content tab, one click away.
//
// Three states exist before any site has a result:
//   loading      the rollup is still on its way
//   unavailable  the rollup failed or was refused (a 403 for a caller without
//                site access). The page does not error: every cell shows a dash
//   ready        the rollup arrived; a site missing from it (archived, never
//                enrolled) also shows the dash

export type AiReadinessRollup =
  | { state: "loading" }
  | { state: "unavailable" }
  | { state: "ready"; bySite: ReadonlyMap<string, FleetAiReadinessSite> };

export const CELL_READY = "Ready";
export const CELL_NOT_CHECKED = "Not checked";
export const CELL_NEEDS_ATTENTION = "Needs attention";
export const CELL_WARNING_ARIA_LABEL = "Open AI connection point";

export function fixLabel(n: number): string {
  return `${n} to fix`;
}

const FLEET_CHECK_LABEL: Record<AiReadinessCheckId, string> = {
  wp_version: "WordPress version",
  abilities_api: "WordPress abilities",
  agent_version: "WPMgr plugin version",
  content_editing: "AI page creation",
  elementor_version: "Elementor version",
  elementor_mcp_switch: "Elementor AI tools switch",
  elementor_atomic: "Elementor Atomic editor",
  bricks_version: "Bricks version",
  bricks_abilities: "Bricks AI abilities",
};

const FLEET_WARNING_SHORT: Record<AiReadinessWarningCode, string> = {
  mcp_adapter_plugin_active: "MCP Adapter plugin",
  elementor_mcp_endpoint_open: "Elementor switch",
};

const OTHER_WARNING_SHORT = "see this site's AI readiness";

export function fleetCheckLabel(id: string): string {
  return Object.prototype.hasOwnProperty.call(FLEET_CHECK_LABEL, id)
    ? FLEET_CHECK_LABEL[id as AiReadinessCheckId]
    : OTHER_CHECK_LABEL;
}

export function fleetWarningLine(code: string): string {
  const short = Object.prototype.hasOwnProperty.call(FLEET_WARNING_SHORT, code)
    ? FLEET_WARNING_SHORT[code as AiReadinessWarningCode]
    : OTHER_WARNING_SHORT;
  return `Open AI connection point: ${short}`;
}

export type AiCell =
  | { kind: "loading" }
  | { kind: "unavailable" }
  | {
      kind: "result";
      tone: StatusTone;
      label: string;
      /** True when the site has an open AI connection point. Never changes `tone`. */
      hasWarning: boolean;
      /** What to fix, then the warnings, one per line. */
      lines: string[];
    };

export function cellFor(rollup: AiReadinessRollup | undefined, siteId: string): AiCell {
  if (!rollup || rollup.state === "unavailable") return { kind: "unavailable" };
  if (rollup.state === "loading") return { kind: "loading" };
  const site = rollup.bySite.get(siteId);
  if (!site) return { kind: "unavailable" };

  const lines: string[] = [];
  let tone: StatusTone;
  let label: string;
  switch (site.status) {
    case "ready":
      tone = "ready";
      label = CELL_READY;
      lines.push("Everything the AI needs on this site is in place.");
      break;
    case "needs_attention":
      tone = "attention";
      label = site.fix_count > 0 ? fixLabel(site.fix_count) : CELL_NEEDS_ATTENTION;
      for (const id of site.failing) lines.push(fleetCheckLabel(id));
      break;
    default:
      // `incomplete`, and any status this page has never heard of.
      tone = "neutral";
      label = CELL_NOT_CHECKED;
      lines.push("Some checks have not run yet.");
  }
  for (const code of site.warnings) lines.push(fleetWarningLine(code));
  return { kind: "result", tone, label, hasWarning: site.warnings.length > 0, lines };
}
