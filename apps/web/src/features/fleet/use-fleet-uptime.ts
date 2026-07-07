// TanStack Query hooks for the fleet-wide uptime endpoints.
// Endpoints: GET /api/v1/fleet/status, GET /api/v1/fleet/incidents

import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

import { fleetKeys } from "./fleet-keys";
import type {
  FleetStatusResponse,
  FleetIncidentsResponse,
  IncidentDetail,
} from "./fleet-types";

const SLOW_THRESHOLD_MS = 2000;

/**
 * Fetch fleet-wide uptime status for all tenant sites.
 * Refetches every 60 s (same cadence as per-site probe worker).
 */
export function useFleetStatus(): UseQueryResult<FleetStatusResponse, Error> {
  return useQuery({
    queryKey: fleetKeys.status(),
    queryFn: async (): Promise<FleetStatusResponse> => {
      const { data, error } = await client.get<FleetStatusResponse, false>({
        url: "/api/v1/fleet/status",
        credentials: "include",
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response from fleet/status");
      return data;
    },
    refetchInterval: 60_000,
    staleTime: 30_000,
  });
}

/**
 * Fetch fleet-wide incidents.
 * @param since - Optional RFC3339 ISO string to filter incidents after this time.
 */
export function useFleetIncidents(
  since?: string,
): UseQueryResult<FleetIncidentsResponse, Error> {
  return useQuery({
    queryKey: fleetKeys.incidents(since),
    queryFn: async (): Promise<FleetIncidentsResponse> => {
      const url = new URL("/api/v1/fleet/incidents", window.location.origin);
      url.searchParams.set("limit", "100");
      if (since) url.searchParams.set("since", since);
      const { data, error } = await client.get<FleetIncidentsResponse, false>({
        url: url.pathname + url.search,
        credentials: "include",
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response from fleet/incidents");
      return data;
    },
    refetchInterval: 60_000,
    staleTime: 30_000,
  });
}

/**
 * Fetch the detail for a single fleet incident (GH #148 detail drill-in):
 * identity, timeline, peak status/last HTTP code/fatal reason, 30d flapping
 * count, and the newest-first probe window.
 *
 * Mirrors `useFleetIncidents`: pass `null` (or an empty string) to disable —
 * the incident-detail dialog only renders this hook's owning component while
 * open, so it never fetches while the modal is closed.
 */
export function useIncidentDetail(
  incidentId: string | null,
): UseQueryResult<IncidentDetail, Error> {
  return useQuery({
    queryKey: fleetKeys.incidentDetail(incidentId ?? ""),
    queryFn: async (): Promise<IncidentDetail> => {
      const { data, error } = await client.get<IncidentDetail, false>({
        url: `/api/v1/fleet/incidents/${encodeURIComponent(incidentId ?? "")}`,
        credentials: "include",
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response from fleet/incidents/:id");
      return data;
    },
    enabled: incidentId !== null && incidentId !== "",
    // Keep the probe window fresh while the incident is still open; a
    // resolved incident's probes/reason/duration never change once closed,
    // so there is nothing to poll for once `ongoing` flips false.
    refetchInterval: (query) => (query.state.data?.ongoing ? 15_000 : false),
  });
}

// Re-export so consumers do not import from fleet-types directly.
export { SLOW_THRESHOLD_MS };
