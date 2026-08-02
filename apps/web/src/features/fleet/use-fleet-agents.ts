// TanStack Query hooks for the agent-release freshness surfaces.
// Endpoints: GET /api/v1/agent/latest, GET /api/v1/fleet/agents.
//
// The manual "check now" trigger for the upstream agent-release mirror
// (GH #322, POST /api/v1/admin/agent-mirror/check) is a superadmin,
// install-level admin-console action, not a tenant-scoped fleet read; its
// hook lives in features/admin/use-admin-agent-mirror.ts, alongside the
// admin console page that renders it
// (routes/_authed/admin/agent-mirror.tsx).

import {
  useQuery,
  type UseQueryResult,
} from "@tanstack/react-query";
import {
  getAgentLatestVersion,
  getFleetAgentVersions,
  type AgentLatestVersion,
  type FleetAgentVersions,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

import { fleetKeys } from "./fleet-keys";

/**
 * Currently published WPMgr agent version. Best-effort on the control plane
 * side (it degrades to `version: "unknown"` rather than erroring when object
 * storage is unavailable or no release has ever been published), so this
 * hook never needs a dedicated error branch for that case, only a real
 * network/auth failure surfaces as `isError`.
 */
export function useAgentLatestVersion(): UseQueryResult<
  AgentLatestVersion,
  Error
> {
  return useQuery({
    queryKey: fleetKeys.agentLatest(),
    queryFn: async (): Promise<AgentLatestVersion> => {
      const { data, error } = await getAgentLatestVersion();
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response from agent/latest");
      return data;
    },
    // Matches the control plane reader's own cache TTL: no point polling
    // faster than the manifest it reads can change.
    staleTime: 5 * 60_000,
  });
}

/**
 * Tenant-wide agent-version rollup: per-site freshness classification
 * (current | outdated | unknown | ineligible) plus fleet counts, and (GH
 * #322) the freshness of the upstream agent-release mirror itself
 * (`agent_mirror`, typed `AgentMirrorStatus`, generated from the OpenAPI
 * contract). Org-scoped only on the control plane (mirrors the
 * vulnerability-scanner fleet rollup); a site-scoped collaborator gets a
 * 403, which surfaces here as `isError` so callers can degrade gracefully
 * (see `AgentFleetSummaryCard` and the Sites page, both of which hide
 * rather than break on it).
 */
export function useFleetAgentVersions(): UseQueryResult<
  FleetAgentVersions,
  Error
> {
  return useQuery({
    queryKey: fleetKeys.agentVersions(),
    queryFn: async (): Promise<FleetAgentVersions> => {
      const { data, error } = await getFleetAgentVersions();
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response from fleet/agents");
      return data;
    },
    staleTime: 60_000,
  });
}
