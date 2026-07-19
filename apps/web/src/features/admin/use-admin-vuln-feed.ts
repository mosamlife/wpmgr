import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toast } from "@/components/toast";
import { toError } from "@/features/auth/use-auth";

// Superadmin-only hooks for the vulnerability feed configuration endpoints.
//
// These are hand-written Gin routes at /api/v1/admin/vuln-feed/* and are NOT
// in the generated OpenAPI SDK. They follow the same pattern as use-admin.ts.
//
// SECURITY NOTE: The plaintext API key is NEVER returned by any endpoint —
// the API stores the age-encrypted key (it must decrypt it to call the feed)
// and exposes status/metadata only, never the key. The key input is
// write-only from the UI perspective; after saving, the input is cleared and
// the UI displays only the status (configured/source/last_synced etc.).

// ---------------------------------------------------------------------------
// Domain types — MUST exactly match the Go handler DTOs.
// ---------------------------------------------------------------------------

/**
 * Source of the active API key.
 * - "ui"  — an operator saved a key via PUT /key
 * - "env" — the instance was started with WPMGR_WORDFENCE_API_KEY set
 * - "none" — no key is configured from any source
 */
export type VulnFeedKeySource = "ui" | "env" | "none";

/**
 * GET /api/v1/admin/vuln-feed/status
 *
 * Status of the Wordfence Intelligence vulnerability feed. The API never
 * returns the plaintext key — only presence, source, and operational health.
 */
export interface VulnFeedStatus {
  /** Whether a key is configured from any source. */
  configured: boolean;
  /** Origin of the active key. */
  source: VulnFeedKeySource;
  /** Whether the last sync completed without a feed-level error. */
  feed_ok: boolean;
  /** Number of vulnerability records currently in the local database. */
  record_count: number;
  /** ISO-8601 timestamp of the last successful sync, or null if never synced. */
  last_synced: string | null;
  /** Human-readable description of the last sync error, or empty string. */
  last_error: string;
  /**
   * True when the feed's CVSS/CVE enrichment pass succeeded on the last
   * sync, independent of `feed_ok` (which tracks scanner-driven detection
   * freshness). False means the last sync was scanner-only: findings may be
   * showing as `unknown` severity fleet-wide even though the feed itself is
   * reachable.
   */
  enrichment_available: boolean;
  /** ISO-8601 timestamp of the last successful enrichment pass, or absent when enrichment has never completed. */
  last_enrichment_at?: string;
}

/**
 * Response from PUT /api/v1/admin/vuln-feed/key
 */
export interface VulnFeedSaveKeyResult {
  ok: true;
  /** True when the CP immediately kicked off a background sync. */
  syncing: boolean;
  /** Non-fatal warning (e.g. key looks valid but feed could not be reached). */
  warning?: string;
}

/**
 * Response from DELETE /api/v1/admin/vuln-feed/key
 */
export interface VulnFeedRemoveKeyResult {
  ok: true;
  /** The source that will now serve as the active key after the UI key was removed. */
  fallback_source: "env" | "none";
}

/**
 * Response from POST /api/v1/admin/vuln-feed/sync
 */
export interface VulnFeedSyncResult {
  ok: true;
  syncing: boolean;
}

// ---------------------------------------------------------------------------
// Query key factory
// ---------------------------------------------------------------------------

export const vulnFeedAdminKeys = {
  status: ["admin", "vuln-feed", "status"] as const,
} as const;

// ---------------------------------------------------------------------------
// Hooks
// ---------------------------------------------------------------------------

/** Fetch the current feed status from GET /api/v1/admin/vuln-feed/status. */
export function useVulnFeedStatus() {
  return useQuery({
    queryKey: vulnFeedAdminKeys.status,
    queryFn: async (): Promise<VulnFeedStatus> => {
      const r = await client.get({ url: "/api/v1/admin/vuln-feed/status" });
      if (r.error) throw toError(r.error);
      return r.data as VulnFeedStatus;
    },
    staleTime: 30_000,
  });
}

/**
 * Save (or replace) the vulnerability feed API key via PUT /api/v1/admin/vuln-feed/key.
 *
 * The key is write-only — the mutation sends it once, and the UI must never
 * echo it back (clear the input immediately in onSuccess). On 422 (bad key
 * format) the error message is shown inline; other errors toast.
 */
export function useVulnFeedSaveKey() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (key: string): Promise<VulnFeedSaveKeyResult> => {
      const r = await client.put({
        url: "/api/v1/admin/vuln-feed/key",
        body: { key },
        headers: { "Content-Type": "application/json" },
      });
      if (r.error) throw toError(r.error);
      return r.data as VulnFeedSaveKeyResult;
    },
    onSuccess: (res) => {
      void qc.invalidateQueries({ queryKey: vulnFeedAdminKeys.status });
      const msg = res.syncing
        ? "Vulnerability feed key saved. Sync started."
        : "Vulnerability feed key saved.";
      toast.success(msg);
      if (res.warning) {
        toast.info("Feed warning", { description: res.warning });
      }
    },
    onError: (err: Error) =>
      toast.error("Failed to save key", { description: err.message }),
  });
}

/**
 * Remove the UI-configured key via DELETE /api/v1/admin/vuln-feed/key.
 * After removal, the feed will fall back to the environment key if one is set,
 * otherwise the feed becomes unconfigured.
 */
export function useVulnFeedRemoveKey() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (): Promise<VulnFeedRemoveKeyResult> => {
      const r = await client.delete({ url: "/api/v1/admin/vuln-feed/key" });
      if (r.error) throw toError(r.error);
      return r.data as VulnFeedRemoveKeyResult;
    },
    onSuccess: (res) => {
      void qc.invalidateQueries({ queryKey: vulnFeedAdminKeys.status });
      const fallbackMsg =
        res.fallback_source === "env"
          ? "Key removed. The environment variable key is now active."
          : "Key removed. The vulnerability feed is now disabled.";
      toast.success(fallbackMsg);
    },
    onError: (err: Error) =>
      toast.error("Failed to remove key", { description: err.message }),
  });
}

/**
 * Trigger an on-demand feed sync via POST /api/v1/admin/vuln-feed/sync.
 * The CP returns 202 — the actual sync happens in the background.
 */
export function useVulnFeedSync() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (): Promise<VulnFeedSyncResult> => {
      const r = await client.post({ url: "/api/v1/admin/vuln-feed/sync" });
      if (r.error) throw toError(r.error);
      return r.data as VulnFeedSyncResult;
    },
    onSuccess: () => {
      toast.success("Feed sync started.");
      // Refetch status after a brief moment so record_count/last_synced update.
      setTimeout(() => {
        void qc.invalidateQueries({ queryKey: vulnFeedAdminKeys.status });
      }, 3_000);
    },
    onError: (err: Error) =>
      toast.error("Sync failed", { description: err.message }),
  });
}
