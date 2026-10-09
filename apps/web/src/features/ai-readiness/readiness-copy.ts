import type {
  AiReadinessCheckId,
  AiReadinessFloors,
  AiReadinessWarningCode,
} from "@wpmgr/api";

import { relativeTime } from "@/lib/utils";

// Copy and row derivation for the per-site "AI readiness" checklist.
//
// Every string the owner reads lives here, keyed on the closed code sets the
// control plane returns (packages/openapi/openapi.yaml, AiReadiness*; emitted
// by apps/api/internal/aireadiness/evaluate.go). The maps below are typed
// Record<AiReadinessCheckId, ...>, so a check id added to the contract and
// regenerated into the client fails typecheck until it has copy here.
//
// A value this module does not recognise is never shown as a failure. An
// unrecognised state, reason or check id renders as a grey "not checked" row,
// because an unknown to this page is not a verdict from the server. The one
// red icon is a state the server named `fail` on a check id this page knows.
//
// No site text reaches this module: the response carries only our own codes
// and version strings the control plane validated. Version strings are still
// only ever rendered as React text nodes.

/** The three icon tones. Grey is never red: an unknown is not a failure. */
export type RowTone = "pass" | "fail" | "neutral";

/**
 * The accessible name of a row's icon. "Not active" and "Unconfirmed" are
 * grey like "Not checked": none of the three is a verdict that something needs
 * fixing.
 */
export type IconLabel =
  | "Passed"
  | "Needs fixing"
  | "Not checked"
  | "Not applicable"
  | "Not active"
  | "Unconfirmed";

/**
 * A check as this page reads it. Wider than the generated AiReadinessCheck on
 * purpose (strings, not unions) so that a code from a newer control plane is a
 * value this module can be handed and must handle, not a type error that only
 * exists at compile time.
 */
export interface CheckInput {
  id: string;
  state: string;
  reason: string | null;
  observed: string | null;
}

export interface CopyContext {
  floors: AiReadinessFloors;
  /** operator+ on this site: the "Turn on" button in AI editing is present. */
  canOperate: boolean;
  /**
   * The builder this row belongs to is installed but not active. Rows that wait
   * on the builder then say it has to be active first, not that it has to be a
   * newer version.
   */
  builderInactive?: boolean;
}

export interface RowView {
  id: string;
  label: string;
  detail: string;
  tone: RowTone;
  iconLabel: IconLabel;
}

/** The icon is a function of the state alone, and only `fail` is red. */
export function iconFor(state: string): { tone: RowTone; label: IconLabel } {
  switch (state) {
    case "pass":
      return { tone: "pass", label: "Passed" };
    case "fail":
      return { tone: "fail", label: "Needs fixing" };
    case "not_applicable":
      return { tone: "neutral", label: "Not applicable" };
    default:
      // `unknown`, and any state this page has never heard of.
      return { tone: "neutral", label: "Not checked" };
  }
}

// ---------------------------------------------------------------------------
// Rows that are grey for a reason of their own
// ---------------------------------------------------------------------------

/** The detail of a builder that is installed and not switched on. */
export const INSTALLED_NOT_ACTIVE = "Installed, not active.";

/**
 * Whether a row says its builder is installed but not active. Only the two
 * builder version rows can. The control plane sends them as not_applicable with
 * the reason `inactive`, which it does not count as a fix (AIReadinessCheck in
 * packages/openapi/openapi.yaml). The state is not checked here: a control plane
 * that still sends the same cause as a fail must not paint the row red, because
 * a builder the owner has not switched on is not a fault. A pass is never read
 * this way.
 */
export function isInactiveRow(c: CheckInput): boolean {
  return (
    (c.id === "elementor_version" || c.id === "bricks_version") &&
    c.state !== "pass" &&
    c.reason === "inactive"
  );
}

/**
 * Rows whose pass or fail is inferred from the site's tool list and has not
 * been confirmed on a real install. Such a row never reads as a confirmed On or
 * Off. Remove an id here once its derivation is confirmed on a licensed
 * install; its row then reads from the server's state like any other.
 */
const UNCONFIRMED_CHECKS: ReadonlySet<string> = new Set(["bricks_abilities"]);

/** The lead of a derived row's detail, and the whole of its label in the fleet hover text. */
export const DERIVED_UNCONFIRMED = "Derived, unconfirmed.";

/**
 * Whether a row's pass or fail is shown as derived and unconfirmed. Unknown and
 * not-applicable states keep their own copy: they claim nothing about On or
 * Off.
 */
export function isUnconfirmedRow(c: CheckInput): boolean {
  return UNCONFIRMED_CHECKS.has(c.id) && (c.state === "pass" || c.state === "fail");
}

function unconfirmedDetail(c: CheckInput): string {
  const seen =
    c.state === "pass"
      ? "Bricks tools are listed on this site."
      : "No Bricks tools are listed on this site.";
  return `${DERIVED_UNCONFIRMED} ${seen} Not yet checked on a licensed Bricks install.`;
}

// ---------------------------------------------------------------------------
// Row labels
// ---------------------------------------------------------------------------

const LABEL: Record<AiReadinessCheckId, (f: AiReadinessFloors) => string> = {
  wp_version: (f) => `WordPress ${f.wp} or later`,
  abilities_api: () => "WordPress abilities",
  agent_version: (f) => `WPMgr plugin ${f.agent} or later`,
  content_editing: () => "AI page creation",
  elementor_version: (f) => `Elementor ${f.elementor} or later`,
  elementor_mcp_switch: () => "Elementor AI tools switch",
  elementor_atomic: () => "Elementor Atomic editor",
  bricks_version: (f) => `Bricks ${f.bricks} or later`,
  bricks_abilities: () => "Bricks AI abilities",
};

/** Shown for a check id this page does not know. */
export const OTHER_CHECK_LABEL = "Another check";
export const OTHER_CHECK_DETAIL = "This page cannot describe this check yet. Reload the page.";

function isCheckId(id: string): id is AiReadinessCheckId {
  return Object.prototype.hasOwnProperty.call(LABEL, id);
}

// ---------------------------------------------------------------------------
// Row details
// ---------------------------------------------------------------------------

const NOT_CHECKED_YET = "Not checked yet.";
const UPDATE_PLUGIN_FOR_FACT = (f: AiReadinessFloors) =>
  `Update the WPMgr plugin to ${f.facts_agent} or later to check this.`;

function observed(c: CheckInput): string | null {
  const v = c.observed?.trim();
  return v ? v : null;
}

/** Fallback by state, for a (check, reason) pair with no specific copy. */
function genericDetail(state: string): string {
  switch (state) {
    case "pass":
      return "Passed.";
    case "fail":
      return "Needs fixing.";
    case "not_applicable":
      return "Not applicable.";
    default:
      return NOT_CHECKED_YET;
  }
}

/**
 * The unknown and not-applicable copy a builder's AI-tools row shares:
 * `elementor_mcp_switch` and `bricks_abilities` differ only in which builder
 * they wait for.
 */
function builderSwitchPending(
  c: CheckInput,
  x: CopyContext,
  builder: "Elementor" | "Bricks",
  needsReason: "needs_elementor" | "needs_bricks",
  floor: string,
): string | undefined {
  switch (c.reason) {
    case "inventory_never_run":
      return NOT_CHECKED_YET;
    case "inventory_truncated":
      return "Could not tell: this site lists more tools than WPMgr reads.";
    case "needs_abilities":
      return "Needs WordPress abilities first.";
    case needsReason:
      return needsBuilder(x, builder, floor);
    default:
      return undefined;
  }
}

/** What a row that waits on its builder says: be active first, or be a newer version first. */
function needsBuilder(x: CopyContext, builder: "Elementor" | "Bricks", floor: string): string {
  return x.builderInactive
    ? `Needs ${builder} to be active first.`
    : `Needs ${builder} ${floor} or later first.`;
}

type DetailFn = (c: CheckInput, x: CopyContext) => string | undefined;

const DETAIL: Record<AiReadinessCheckId, DetailFn> = {
  wp_version: (c, x) => {
    const v = observed(c);
    if (c.state === "pass") return v ? `WordPress ${v}.` : undefined;
    if (c.state === "fail") {
      const need = `Builder tools need ${x.floors.wp} or later. Update WordPress on this site.`;
      return v ? `WordPress ${v}. ${need}` : need;
    }
    if (c.reason === "not_reported") return "WordPress has not reported its version yet.";
    return undefined;
  },

  abilities_api: (c) => {
    if (c.state === "pass") return "Available.";
    if (c.state === "fail") return "Not available. WordPress 6.9 or later includes it.";
    switch (c.reason) {
      case "inventory_never_run":
        return NOT_CHECKED_YET;
      case "agent_too_old":
        return "Update the WPMgr plugin to check this.";
      case "not_reported":
        return "WordPress has not reported its version yet.";
      default:
        return undefined;
    }
  },

  agent_version: (c, x) => {
    const v = observed(c);
    if (c.state === "pass") return v ? `Version ${v}.` : undefined;
    if (c.state === "fail") {
      const need = `Update the WPMgr plugin to ${x.floors.agent} or later.`;
      return v ? `Version ${v}. ${need}` : need;
    }
    if (c.reason === "not_reported") return "The WPMgr plugin has not reported its version yet.";
    return undefined;
  },

  content_editing: (c, x) => {
    if (c.state === "pass") return "On.";
    if (c.state === "fail") {
      return x.canOperate
        ? "Off. Turn it on in AI editing, below."
        : "Off. An operator can turn it on in AI editing, below.";
    }
    return undefined;
  },

  elementor_version: (c, x) => {
    const v = observed(c);
    if (c.state === "pass") return v ? `Elementor ${v}, active.` : undefined;
    if (c.state === "fail") {
      if (c.reason === "too_old") {
        const need = `Its AI tools need ${x.floors.elementor} or later. Update Elementor.`;
        return v ? `Elementor ${v}. ${need}` : need;
      }
      return undefined;
    }
    if (c.reason === "not_reported") return "Elementor has not reported its version yet.";
    return undefined;
  },

  elementor_mcp_switch: (c, x) => {
    if (c.state === "pass") return "On.";
    if (c.state === "fail") {
      return "Off. A site administrator turns it on in the WordPress admin, in Elementor's MCP settings.";
    }
    return builderSwitchPending(c, x, "Elementor", "needs_elementor", x.floors.elementor);
  },

  elementor_atomic: (c, x) => {
    if (c.state === "pass") return "On.";
    if (c.state === "fail") {
      return "Off. Without it the AI can only create a blank Elementor page. A site administrator turns it on in the WordPress admin: Elementor → Settings → Atomic Editor.";
    }
    switch (c.reason) {
      case "agent_too_old_for_fact":
        return UPDATE_PLUGIN_FOR_FACT(x.floors);
      case "not_reported":
        return "Not reported yet.";
      case "needs_elementor":
        return needsBuilder(x, "Elementor", x.floors.elementor);
      default:
        return undefined;
    }
  },

  bricks_version: (c, x) => {
    const v = observed(c);
    if (c.state === "pass") return v ? `Bricks ${v}, active theme.` : undefined;
    if (c.state === "fail") {
      if (c.reason === "too_old") {
        const need = `Its AI abilities need ${x.floors.bricks} or later. Update Bricks.`;
        return v ? `Bricks ${v}. ${need}` : need;
      }
      return undefined;
    }
    if (c.reason === "agent_too_old_for_fact") return UPDATE_PLUGIN_FOR_FACT(x.floors);
    if (c.reason === "not_reported") return "Not reported yet.";
    return undefined;
  },

  bricks_abilities: (c, x) => {
    if (c.state === "pass") {
      return "On. Bricks advises keeping this off on live sites while it is experimental.";
    }
    if (c.state === "fail") {
      return "Off. A site administrator turns it on in the WordPress admin: Bricks → AI.";
    }
    return builderSwitchPending(c, x, "Bricks", "needs_bricks", x.floors.bricks);
  },
};

/** One row of the checklist, ready to render. */
export function describeCheck(c: CheckInput, x: CopyContext): RowView {
  if (!isCheckId(c.id)) {
    // A check this page was not built for. Neutral whatever the state, so a
    // code from a newer control plane never paints a red cross it cannot explain.
    return {
      id: c.id,
      label: OTHER_CHECK_LABEL,
      detail: OTHER_CHECK_DETAIL,
      tone: "neutral",
      iconLabel: "Not checked",
    };
  }
  const label = LABEL[c.id](x.floors);
  if (isInactiveRow(c)) {
    return { id: c.id, label, detail: INSTALLED_NOT_ACTIVE, tone: "neutral", iconLabel: "Not active" };
  }
  if (isUnconfirmedRow(c)) {
    return { id: c.id, label, detail: unconfirmedDetail(c), tone: "neutral", iconLabel: "Unconfirmed" };
  }
  const icon = iconFor(c.state);
  return {
    id: c.id,
    label,
    detail: DETAIL[c.id](c, x) ?? genericDetail(c.state),
    tone: icon.tone,
    iconLabel: icon.label,
  };
}

// ---------------------------------------------------------------------------
// Status line, freshness line, warnings
// ---------------------------------------------------------------------------

export type StatusTone = "ready" | "attention" | "neutral";

export function statusLine(status: string, fixCount: number): { tone: StatusTone; text: string } {
  switch (status) {
    case "ready":
      return { tone: "ready", text: "Ready. Everything the AI needs on this site is in place." };
    case "needs_attention":
      return {
        tone: "attention",
        text:
          fixCount === 1
            ? `${fixCount} thing to fix before the AI can work here.`
            : `${fixCount} things to fix before the AI can work here.`,
      };
    default:
      // `incomplete`, and any status this page has never heard of.
      return { tone: "neutral", text: "Some checks have not run yet." };
  }
}

/** "Site details from 5m ago. Tool list from 2h ago." with its two null forms. */
export function freshnessLine(
  metadataAsOf: string | null,
  abilitiesAsOf: string | null,
  now: number = Date.now(),
): string {
  const details = relativeTime(metadataAsOf, now);
  const tools = relativeTime(abilitiesAsOf, now);
  return [
    details ? `Site details from ${details}.` : "Site details not reported yet.",
    tools ? `Tool list from ${tools}.` : "Tool list not read yet.",
  ].join(" ");
}

export const WARNING_COPY: Record<AiReadinessWarningCode, string> = {
  mcp_adapter_plugin_active:
    "The WordPress MCP Adapter plugin is active on this site. It lets any logged-in user of the site connect an AI tool directly, without WPMgr's approvals, undo or audit. WPMgr does not need it. If nobody uses it, consider deactivating it.",
  elementor_mcp_endpoint_open:
    "Elementor's AI tools switch also opens Elementor's own AI connection point on this site. Any logged-in user can connect an AI tool to it, without WPMgr's approvals, undo or audit.",
};

/** Shown for a warning code this page does not know. */
export const OTHER_WARNING_COPY =
  "WPMgr reported a notice about this site that this page cannot show yet. Reload the page.";

export function warningCopy(code: string): string {
  return Object.prototype.hasOwnProperty.call(WARNING_COPY, code)
    ? WARNING_COPY[code as AiReadinessWarningCode]
    : OTHER_WARNING_COPY;
}

// ---------------------------------------------------------------------------
// Groups
// ---------------------------------------------------------------------------

export const BASE_GROUP_TITLE = "WordPress and WPMgr";

const BUILDER_NAME: Record<string, string> = {
  elementor: "Elementor",
  bricks: "Bricks",
};

export interface GroupHeading {
  title: string;
  /** Builder groups only: the name used in the sentences below the title. */
  builder: string | null;
}

export function groupHeading(id: string): GroupHeading {
  if (id === "base") return { title: BASE_GROUP_TITLE, builder: null };
  const builder = Object.prototype.hasOwnProperty.call(BUILDER_NAME, id) ? BUILDER_NAME[id]! : null;
  return { title: builder ?? "Other checks", builder };
}

/** Right side of a builder group's header. */
export function builderHeaderRight(g: { installed?: boolean; version?: string | null }): string {
  if (!g.installed) return "Not installed";
  const v = g.version?.trim();
  return v ? `Version ${v}` : "Installed";
}

export function notInstalledLine(builder: string): string {
  return `${builder} is not installed on this site. Nothing to check.`;
}

/** Shown under a builder's checks while WPMgr cannot build with that builder yet. */
export function comingNote(builder: string): string {
  return `WPMgr cannot build ${builder} pages yet. These checks show whether this site will be ready.`;
}

// ---------------------------------------------------------------------------
// Refresh ("Check again") messages
// ---------------------------------------------------------------------------

export const REFRESH_ASKED = "Asked the site to report again. Results update within a couple of minutes.";
export const REFRESH_UNREACHABLE =
  "WPMgr could not reach this site, so nothing was checked. Try again when the site is back online.";
export const REFRESH_FORBIDDEN = "You do not have permission to run this check.";

export function refreshOtherCopy(serverMessage: string): string {
  return `Could not ask the site to report. ${serverMessage}`;
}

// ---------------------------------------------------------------------------
// Load failure
// ---------------------------------------------------------------------------

export const LOAD_ERROR_WHAT = "Could not load AI readiness.";
export const LOAD_ERROR_NOT_FOUND =
  "This site is not connected to WPMgr, so there is nothing to check yet.";
