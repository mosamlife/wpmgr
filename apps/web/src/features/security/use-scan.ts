import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// S3 — Malware / file-integrity scan data hooks.
//
// The scan endpoints are hand-rolled Gin routes (not in the generated @wpmgr/api
// SDK). We call them via `client.get()` / `client.post()` from the configured
// Hey API client (`@wpmgr/api`). That client is pre-configured with:
//   - baseUrl: "" (same-origin, Vite dev proxy / nginx in prod)
//   - credentials: "include" (sends the HttpOnly wpmgr_session cookie)
// So authentication is identical to every generated SDK call — session cookie,
// no Bearer token needed.
//
// All paths use the /api/v1 prefix that the nginx / Vite proxy routes to the
// backend. Each `url` below is the full path starting with /api/v1/… which is
// exactly what the generated SDK operations do (e.g. /api/v1/sites/{siteId}).

// ---------------------------------------------------------------------------
// Domain types (hand-rolled, no ogen codegen)
// ---------------------------------------------------------------------------

export type ScanStatus = "queued" | "scanning" | "diffing" | "done" | "failed";
export type ScanFindingType =
  | "core_modified"
  | "core_missing"
  | "core_unknown_injected";
export type ScanFindingSeverity = "high" | "medium" | "low";

export interface ScanRun {
  id: string;
  kind: string;
  status: ScanStatus;
  files_scanned: number | null;
  wp_version: string | null;
  locale: string | null;
  error: string | null;
  // Nullable: a nil Go map marshals to JSON null, so this can arrive null even
  // though the DB column defaults to '{}'. Guard before Object.values()/entries().
  finding_counts: Record<string, number> | null;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface ScanFinding {
  id: string;
  run_id: string;
  finding_type: ScanFindingType;
  path: string;
  severity: ScanFindingSeverity;
  expected_md5: string | null;
  actual_md5: string | null;
  ignored: boolean;
  created_at: string;
}

export interface ScanFileResult {
  ok: boolean;
  path: string;
  size: number;
  content_base64: string;
}

// ---------------------------------------------------------------------------
// Cache key family
// ---------------------------------------------------------------------------

export const scanKeys = {
  all: ["scans"] as const,
  runsForSite: (siteId: string) => ["scans", "runs", siteId] as const,
  run: (siteId: string, runId: string) =>
    ["scans", "run", siteId, runId] as const,
  findings: (siteId: string, runId: string) =>
    ["scans", "findings", siteId, runId] as const,
};

/** In-progress scan states where we want to live-poll. */
const LIVE_STATUSES = new Set<ScanStatus>(["queued", "scanning", "diffing"]);

export function isLiveScan(status: ScanStatus): boolean {
  return LIVE_STATUSES.has(status);
}

// ---------------------------------------------------------------------------
// Helper — low-level authenticated request via the configured Hey API client.
// The client sends credentials: "include" (session cookie) just like the SDK.
// ---------------------------------------------------------------------------

async function apiGet<T>(url: string): Promise<T> {
  const result = await client.get({ url });
  if (result.error !== undefined) throw toError(result.error);
  return result.data as T;
}

async function apiPost<T>(url: string, body?: unknown): Promise<T> {
  const result = await client.post({
    url,
    body,
    headers: { "Content-Type": "application/json" },
  });
  if (result.error !== undefined) throw toError(result.error);
  return result.data as T;
}

// ---------------------------------------------------------------------------
// useScanRuns — GET /api/v1/sites/{siteId}/scans
// ---------------------------------------------------------------------------

export function useScanRuns(
  siteId: string,
): UseQueryResult<ScanRun[], Error> {
  return useQuery({
    queryKey: scanKeys.runsForSite(siteId),
    queryFn: async () => {
      const data = await apiGet<{ items: ScanRun[] }>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/scans`,
      );
      return data.items ?? [];
    },
    // Poll while any run is in progress (a new scan may have just been started).
    refetchInterval: (query) => {
      const items = query.state.data ?? [];
      return items.some((r) => isLiveScan(r.status)) ? 4000 : false;
    },
  });
}

// ---------------------------------------------------------------------------
// useScanRun — GET /api/v1/sites/{siteId}/scans/{runId}
// Polls while status is queued|scanning|diffing.
// ---------------------------------------------------------------------------

export function useScanRun(
  siteId: string,
  runId: string,
): UseQueryResult<ScanRun, Error> {
  return useQuery({
    queryKey: scanKeys.run(siteId, runId),
    queryFn: async () =>
      apiGet<ScanRun>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/scans/${encodeURIComponent(runId)}`,
      ),
    enabled: Boolean(siteId && runId),
    refetchInterval: (query) => {
      const data = query.state.data;
      if (!data) return false;
      return isLiveScan(data.status) ? 2000 : false;
    },
  });
}

// ---------------------------------------------------------------------------
// useStartScan — POST /api/v1/sites/{siteId}/scans
// ---------------------------------------------------------------------------

export function useStartScan(
  siteId: string,
): UseMutationResult<ScanRun, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () =>
      apiPost<ScanRun>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/scans`,
        { kind: "core" },
      ),
    onSuccess: (newRun) => {
      // Optimistically prepend the new run to the list so the UI is responsive.
      queryClient.setQueryData<ScanRun[]>(
        scanKeys.runsForSite(siteId),
        (prev) => (prev ? [newRun, ...prev] : [newRun]),
      );
      // Seed the run detail cache so useScanRun can start polling immediately.
      queryClient.setQueryData(scanKeys.run(siteId, newRun.id), newRun);
      // Kick a background invalidation to confirm server state.
      void queryClient.invalidateQueries({
        queryKey: scanKeys.runsForSite(siteId),
      });
    },
  });
}

// ---------------------------------------------------------------------------
// useScanFindings — GET /api/v1/sites/{siteId}/scans/{runId}/findings
// ---------------------------------------------------------------------------

export function useScanFindings(
  siteId: string,
  runId: string,
): UseQueryResult<ScanFinding[], Error> {
  return useQuery({
    queryKey: scanKeys.findings(siteId, runId),
    queryFn: async () => {
      const data = await apiGet<{ items: ScanFinding[] }>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/scans/${encodeURIComponent(runId)}/findings`,
      );
      return data.items ?? [];
    },
    enabled: Boolean(siteId && runId),
    staleTime: 30_000,
  });
}

// ---------------------------------------------------------------------------
// useIgnoreFinding — POST /api/v1/findings/{findingId}/ignore
// ---------------------------------------------------------------------------

export function useIgnoreFinding(): UseMutationResult<
  ScanFinding,
  Error,
  { findingId: string; siteId: string; runId: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ findingId }) =>
      apiPost<ScanFinding>(
        `/api/v1/findings/${encodeURIComponent(findingId)}/ignore`,
      ),
    onSuccess: (updated, { siteId, runId }) => {
      // Patch the findings list in the cache to reflect the toggled state.
      queryClient.setQueryData<ScanFinding[]>(
        scanKeys.findings(siteId, runId),
        (prev) =>
          prev
            ? prev.map((f) => (f.id === updated.id ? updated : f))
            : prev,
      );
    },
  });
}

// ---------------------------------------------------------------------------
// useFindingFile — POST /api/v1/sites/{siteId}/scans/{runId}/findings/{findingId}/file
// On-demand (lazy) — call mutate() explicitly; not a background query.
// ---------------------------------------------------------------------------

export function useFindingFile(): UseMutationResult<
  ScanFileResult,
  Error,
  { siteId: string; runId: string; findingId: string }
> {
  return useMutation({
    mutationFn: async ({ siteId, runId, findingId }) =>
      apiPost<ScanFileResult>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/scans/${encodeURIComponent(runId)}/findings/${encodeURIComponent(findingId)}/file`,
      ),
  });
}
