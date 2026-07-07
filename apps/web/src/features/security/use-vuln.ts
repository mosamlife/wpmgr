import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// Security Suite Phase 4 — vulnerability scanner data hooks.
//
// The vulnerability endpoints are hand-written Gin routes (NOT in the generated
// @wpmgr/api SDK). We call them via `client.get()` / `client.post()` from the
// configured Hey API client exactly as `use-scan.ts` and `use-hardening.ts` do.
//
// The client is pre-configured (lib/api.ts):
//   - baseUrl: ""   (same-origin; Vite dev proxy / nginx in prod)
//   - credentials: "include"  (sends the HttpOnly wpmgr_session cookie)
//
// All paths begin with /api/v1 — the prefix the Vite proxy routes to the CP.
//
// DTO shapes MUST exactly match apps/api/internal/vuln/handler.go findingDTO +
// fleetFindingDTO + attributionDTO + the two response wrappers. A mismatch here
// was the class of bug that caused the 0.52.1 prod white-screen.

// ---------------------------------------------------------------------------
// Domain types — MUST match Go DTOs exactly (handler.go lines 89-147)
// ---------------------------------------------------------------------------

/** Severity values: must match Go `site_vulnerabilities.severity` column. */
export type VulnSeverity = "critical" | "high" | "medium" | "low";

/** Status values: must match Go `site_vulnerabilities.status` column. */
export type VulnStatus = "open" | "dismissed" | "remediated";

/** Kind values: must match Go `site_vulnerabilities.kind` column. */
export type VulnKind = "plugin" | "theme" | "core";

/**
 * A single vulnerability finding — maps 1:1 to `findingDTO` in handler.go.
 *
 * Nullable optional fields use `| null` (Go `omitempty` emits the field
 * absent / null on JSON, both must be handled safely).
 */
export interface VulnFinding {
  /** UUID string — finding row id in `site_vulnerabilities`. */
  id: string;
  site_id: string;
  /** Wordfence Intelligence record UUID. */
  vuln_id: string;
  kind: VulnKind;
  slug: string;
  name: string;
  installed_version: string;
  /** Null / absent when no patched version is known. */
  fixed_version?: string | null;
  severity: VulnSeverity;
  /** Null when not available in the feed for this record. */
  cvss_score?: number | null;
  /** CVE identifier string, e.g. "CVE-2024-12345". Null when not assigned. */
  cve?: string | null;
  /** Direct link to the CVE record. Null when cve is null. */
  cve_link?: string | null;
  title: string;
  status: VulnStatus;
  /** RFC 3339 timestamp string. */
  first_seen: string;
  /** RFC 3339 timestamp string. */
  last_seen: string;
  /** Wordfence Intelligence reference URLs for the link-back attribution gate. */
  references: string[];
}

/**
 * Attribution notices — maps to `attributionDTO` in handler.go.
 * Sourced from `wordfence_vuln_feed_meta` (single-row sentinel).
 *
 * GATE 0 (legally required): DefiantNotice + DefiantLicense must appear in
 * the UI footer on every vuln view; MitreNotice must appear on any row
 * that shows a CVE id.
 */
export interface VulnAttribution {
  defiant_notice: string;
  defiant_license: string;
  mitre_notice: string;
}

/**
 * Per-site GET /api/v1/sites/:siteId/vulnerabilities response.
 * Maps to `siteVulnsResponseDTO` in handler.go.
 */
export interface SiteVulnsResponse {
  items: VulnFinding[];
  attribution: VulnAttribution;
  feed_ok: boolean;
  /** RFC 3339 string or absent when the feed has never been synced. */
  feed_synced?: string | null;
}

/**
 * Fleet item — maps to `fleetFindingDTO` in handler.go.
 */
export interface FleetVulnFinding {
  site_id: string;
  site_name: string;
  site_url: string;
  /** The full finding detail nested inside. */
  finding: VulnFinding;
}

/**
 * Fleet GET /api/v1/vulnerabilities response.
 * Maps to `fleetVulnsResponseDTO` in handler.go.
 */
export interface FleetVulnsResponse {
  total_open: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  items: FleetVulnFinding[];
  attribution: VulnAttribution;
  feed_ok: boolean;
  feed_synced?: string | null;
}

/** POST /rescan response — maps to `rescanResponseDTO`. */
export interface RescanResponse {
  ok: boolean;
}

/** POST /:id/remediate response — maps to `remediateResponseDTO`. */
export interface RemediateResponse {
  run_id: string;
}

// ---------------------------------------------------------------------------
// Cache key factory
// ---------------------------------------------------------------------------

export const vulnKeys = {
  all: ["vulns"] as const,
  /** Fleet rollup for the current tenant. */
  fleet: () => ["vulns", "fleet"] as const,
  /** All per-site vulnerability lists. */
  siteLists: () => ["vulns", "site"] as const,
  /** Per-site vulnerability list. */
  site: (siteId: string) => ["vulns", "site", siteId] as const,
};

// ---------------------------------------------------------------------------
// Low-level authenticated helpers (same pattern as use-scan.ts / use-hardening.ts)
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
  // 204 No Content — the body is null/undefined; the Go handler uses c.Status(204).
  // Treat a null body as a successful empty result so callers don't see a false error.
  if (result.error !== undefined) throw toError(result.error);
  return result.data as T;
}

// ---------------------------------------------------------------------------
// useSiteVulnerabilities — GET /api/v1/sites/{siteId}/vulnerabilities
// ---------------------------------------------------------------------------

export function useSiteVulnerabilities(
  siteId: string,
): UseQueryResult<SiteVulnsResponse, Error> {
  return useQuery({
    queryKey: vulnKeys.site(siteId),
    queryFn: async () =>
      apiGet<SiteVulnsResponse>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/vulnerabilities`,
      ),
    staleTime: 30_000,
    enabled: Boolean(siteId),
  });
}

// ---------------------------------------------------------------------------
// useFleetVulnerabilities — GET /api/v1/vulnerabilities
// ---------------------------------------------------------------------------

export function useFleetVulnerabilities(): UseQueryResult<
  FleetVulnsResponse,
  Error
> {
  return useQuery({
    queryKey: vulnKeys.fleet(),
    queryFn: async () =>
      apiGet<FleetVulnsResponse>("/api/v1/vulnerabilities"),
    staleTime: 30_000,
  });
}

// ---------------------------------------------------------------------------
// useRescanVulns — POST /api/v1/sites/{siteId}/vulnerabilities/rescan
// ---------------------------------------------------------------------------

export function useRescanVulns(
  siteId: string,
): UseMutationResult<RescanResponse, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () =>
      apiPost<RescanResponse>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/vulnerabilities/rescan`,
      ),
    onSuccess: () => {
      // Optimistically refresh the site list and the fleet summary so counts
      // update after the rescan is enqueued. The actual findings update is
      // async (River worker), but a stale-time-busting invalidation is the
      // correct signal to refetch when the user asks for fresh data.
      void queryClient.invalidateQueries({ queryKey: vulnKeys.site(siteId) });
      void queryClient.invalidateQueries({ queryKey: vulnKeys.fleet() });
    },
  });
}

// ---------------------------------------------------------------------------
// useDismissVuln — POST /api/v1/sites/{siteId}/vulnerabilities/{id}/dismiss
// ---------------------------------------------------------------------------

export function useDismissVuln(
  siteId: string,
): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (findingId: string) => {
      await apiPost<null>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/vulnerabilities/${encodeURIComponent(findingId)}/dismiss`,
      );
    },
    onSuccess: (_data, findingId) => {
      // Patch the cached finding to status=dismissed so the UI updates
      // immediately without waiting for a full refetch.
      queryClient.setQueryData<SiteVulnsResponse>(
        vulnKeys.site(siteId),
        (prev) => {
          if (!prev) return prev;
          const dismissed: VulnStatus = "dismissed";
          return {
            ...prev,
            items: prev.items.map((f) =>
              f.id === findingId ? { ...f, status: dismissed } : f,
            ),
          };
        },
      );
      void queryClient.invalidateQueries({ queryKey: vulnKeys.fleet() });
    },
  });
}

// ---------------------------------------------------------------------------
// useRestoreVuln — POST /api/v1/sites/{siteId}/vulnerabilities/{id}/restore
// ---------------------------------------------------------------------------

export function useRestoreVuln(
  siteId: string,
): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (findingId: string) => {
      await apiPost<null>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/vulnerabilities/${encodeURIComponent(findingId)}/restore`,
      );
    },
    onSuccess: (_data, findingId) => {
      queryClient.setQueryData<SiteVulnsResponse>(
        vulnKeys.site(siteId),
        (prev) => {
          if (!prev) return prev;
          const open: VulnStatus = "open";
          return {
            ...prev,
            items: prev.items.map((f) =>
              f.id === findingId ? { ...f, status: open } : f,
            ),
          };
        },
      );
      void queryClient.invalidateQueries({ queryKey: vulnKeys.fleet() });
    },
  });
}

// ---------------------------------------------------------------------------
// useRemediateVuln — POST /api/v1/sites/{siteId}/vulnerabilities/{id}/remediate
//
// The CP maps the finding to `update.CreateRun` for the single site
// (apps/api/internal/vuln/handler.go:remediate, service.go:Remediate). Returns
// the run_id of the created update run, which the caller can hand to
// `useUpdateRun` / `useRunEventStream` for live progress — the same pattern
// used by `use-row-update.ts` on the Updates page. The vuln finding itself is
// NOT immediately removed from cache; it transitions to "remediated" status only
// after the RescanSite worker re-runs (triggered by the update run completion
// hook on the CP side).
// ---------------------------------------------------------------------------

export function useRemediateVuln(
  siteId: string,
): UseMutationResult<RemediateResponse, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (findingId: string) =>
      apiPost<RemediateResponse>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/vulnerabilities/${encodeURIComponent(findingId)}/remediate`,
      ),
    onSuccess: () => {
      // Invalidate vulns so the card refreshes once the CP re-scans after the
      // update run completes. The update run's SSE stream handles progress; here
      // we just ensure the vuln list eventually reflects the resolved state.
      void queryClient.invalidateQueries({ queryKey: vulnKeys.site(siteId) });
      void queryClient.invalidateQueries({ queryKey: vulnKeys.fleet() });
    },
  });
}

// ---------------------------------------------------------------------------
// Helpers — exported for tests and components
// ---------------------------------------------------------------------------

/** True when the severity warrants a dangerous/urgent colour (auto-expand card). */
export function isHighRisk(severity: VulnSeverity): boolean {
  return severity === "critical" || severity === "high";
}

/** Returns the count of open findings with critical or high severity. */
export function countHighRisk(findings: VulnFinding[]): number {
  return findings.filter(
    (f) => f.status === "open" && isHighRisk(f.severity),
  ).length;
}

/** Severity ranked worst-first, for picking the single worst severity to display. */
const SEVERITY_RANK: VulnSeverity[] = ["critical", "high", "medium", "low"];

/**
 * Returns the worst (highest-priority) severity among the given findings, or
 * `undefined` when the list is empty. Used by summary tiles (e.g. the Health
 * tab's Vulnerabilities tile) that show one representative severity rather
 * than the full findings table.
 */
export function worstSeverity(
  findings: VulnFinding[],
): VulnSeverity | undefined {
  for (const severity of SEVERITY_RANK) {
    if (findings.some((f) => f.severity === severity)) return severity;
  }
  return undefined;
}

/**
 * Validates a feed-supplied URL before it is placed in an <a href>.
 *
 * Feed records come from an external source (Wordfence Intelligence) and are
 * attacker-influenceable. A malicious feed record could carry a `javascript:`
 * or `data:` URL that executes script in the operator session on click.
 *
 * Only `http://` and `https://` schemes are allowed. Every other scheme
 * (including `javascript:`, `data:`, `vbscript:`, bare paths, etc.) returns
 * `undefined`, which callers must treat as "render plain text instead of an
 * anchor".
 *
 * Returns the original string when it is safe, or `undefined` when it is not.
 */
export function safeExternalHref(u?: string | null): string | undefined {
  if (!u) return undefined;
  if (u.startsWith("https://") || u.startsWith("http://")) return u;
  return undefined;
}
