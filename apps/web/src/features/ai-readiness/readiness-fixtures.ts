import type {
  AiReadinessCheck,
  AiReadinessCheckId,
  AiReadinessFloors,
  AiReadinessGroup,
  AiReadinessWarningCode,
  FleetAiReadinessSite,
  SiteAiReadiness,
} from "@wpmgr/api";

// Test fixtures for the AI readiness surfaces, shaped like what the control
// plane writes (apps/api/internal/aireadiness/dto.go):
//   - every check carries `reason` and `observed`, null when there is none;
//   - the base group has only `id` and `checks`;
//   - a builder group always has `installed`, `version` (null when not
//     installed or unusable) and `wpmgr_support`, and `checks` is [] when the
//     builder is not installed.
// The floors are the control plane's own constants:
//   MinWPVersionForVendorReads            "7.1"     agentcmd/ability_run_vendor.go
//   MinAgentVersionForRestCall / VendorReads "0.61.158" agentcmd/ability_run_*.go
//   MinAgentVersionForBuilderFacts        "0.61.159" agentcmd/builder_facts_contract.go
//   MinElementorVersion, MinBricksVersion "4.3", "2.4" aireadiness/evaluate.go

export const FLOORS: AiReadinessFloors = {
  wp: "7.1",
  agent: "0.61.158",
  facts_agent: "0.61.159",
  elementor: "4.3",
  bricks: "2.4",
};

export const SITE_ID = "11111111-0000-0000-0000-000000000001";

type State = AiReadinessCheck["state"];
type Reason = NonNullable<AiReadinessCheck["reason"]>;

export function chk(
  id: AiReadinessCheckId,
  state: State,
  reason: Reason | null = null,
  observed: string | null = null,
): AiReadinessCheck {
  return { id, state, reason, observed };
}

/** The four base rows, all passing. */
export function baseChecks(over: Partial<Record<AiReadinessCheckId, AiReadinessCheck>> = {}): AiReadinessCheck[] {
  const rows: AiReadinessCheck[] = [
    chk("wp_version", "pass", null, "7.1"),
    chk("abilities_api", "pass"),
    chk("agent_version", "pass", null, "0.61.159"),
    chk("content_editing", "pass"),
  ];
  return rows.map((r) => over[r.id] ?? r);
}

export function baseGroup(checks: AiReadinessCheck[] = baseChecks()): AiReadinessGroup {
  return { id: "base", checks };
}

export function builderGroup(
  id: "elementor" | "bricks",
  opts: {
    installed: boolean;
    version?: string | null;
    support?: "coming" | "available";
    checks?: AiReadinessCheck[];
  },
): AiReadinessGroup {
  return {
    id,
    installed: opts.installed,
    version: opts.installed ? (opts.version ?? null) : null,
    wpmgr_support: opts.support ?? "coming",
    checks: opts.installed ? (opts.checks ?? []) : [],
  };
}

export function elementorAllPassing(version = "4.3.4"): AiReadinessGroup {
  return builderGroup("elementor", {
    installed: true,
    version,
    checks: [
      chk("elementor_version", "pass", null, version),
      chk("elementor_mcp_switch", "pass"),
      chk("elementor_atomic", "pass"),
    ],
  });
}

export function bricksAllPassing(version = "2.4.1"): AiReadinessGroup {
  return builderGroup("bricks", {
    installed: true,
    version,
    checks: [chk("bricks_version", "pass", null, version), chk("bricks_abilities", "pass")],
  });
}

/** A site where everything passes and neither builder is installed. */
export function readiness(over: Partial<SiteAiReadiness> = {}): SiteAiReadiness {
  return {
    site_id: SITE_ID,
    status: "ready",
    fix_count: 0,
    metadata_as_of: new Date(Date.now() - 5 * 60_000).toISOString(),
    abilities_as_of: new Date(Date.now() - 2 * 3_600_000).toISOString(),
    warnings: [],
    floors: FLOORS,
    groups: [
      baseGroup(),
      builderGroup("elementor", { installed: false }),
      builderGroup("bricks", { installed: false }),
    ],
    ...over,
  };
}

/** The hey-api result tuple for a 200. */
export function okResult<T>(data: T) {
  return { data, error: undefined, response: { status: 200 } };
}

/** The hey-api result tuple for a failed call, with the control plane's flat error body. */
export function failResult(status: number, code: string, message: string) {
  return { data: undefined, error: { code, message }, response: { status } };
}

export function fleetSite(
  over: Partial<FleetAiReadinessSite> & { site_id: string },
): FleetAiReadinessSite {
  return {
    status: "ready",
    fix_count: 0,
    failing: [],
    warnings: [],
    ...over,
  };
}

export const ADAPTER_WARNING: AiReadinessWarningCode = "mcp_adapter_plugin_active";
export const ELEMENTOR_WARNING: AiReadinessWarningCode = "elementor_mcp_endpoint_open";
