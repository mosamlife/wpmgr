import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

import type {
  AssetSelectionBody,
  BatchResponse,
  CancelResponse,
  ListAssetsResponse,
  OptimizeBody,
  SyncResponse,
} from "../types";

// Server-state hooks for the Media Optimizer asset list + the action mutations.
//
// Like use-site-connection.ts, these routes are NOT in the generated @wpmgr/api
// SDK (hand-rolled DTOs — ADR-043 §7), so we call the shared `client` directly
// with explicit `url`s. The response-style generic `<{ 200: T }>` mirrors how
// use-sites.ts unwraps the SDK's responses-map so `data` types as `T`.
//
// Endpoints (handler.go):
//   GET  /api/v1/sites/:siteId/media/assets?cursor&limit&status&format&search
//   POST /api/v1/sites/:siteId/media/sync
//   POST /api/v1/sites/:siteId/media/optimize
//   POST /api/v1/sites/:siteId/media/restore
//   POST /api/v1/sites/:siteId/media/delete-originals
//   POST /api/v1/sites/:siteId/media/cancel

export interface AssetFilters {
  status?: string;
  format?: string;
  search?: string;
  limit?: number;
  cursor?: string;
}

export const mediaKeys = {
  all: ["media"] as const,
  assets: (siteId: string, filters: AssetFilters = {}) =>
    [...mediaKeys.all, "assets", siteId, filters] as const,
  jobs: (siteId: string, state?: string) =>
    [...mediaKeys.all, "jobs", siteId, state ?? "all"] as const,
  job: (jobId: string) => [...mediaKeys.all, "job", jobId] as const,
};

function base(siteId: string): string {
  return `/api/v1/sites/${encodeURIComponent(siteId)}/media`;
}

/** List a site's media assets + the summary rollup (handler.listAssets). */
export function useMediaAssets(
  siteId: string,
  filters: AssetFilters = {},
): UseQueryResult<ListAssetsResponse, Error> {
  return useQuery({
    queryKey: mediaKeys.assets(siteId, filters),
    queryFn: async () => {
      const query: Record<string, string> = {};
      if (filters.status) query.status = filters.status;
      if (filters.format) query.format = filters.format;
      if (filters.search) query.search = filters.search;
      if (filters.cursor) query.cursor = filters.cursor;
      if (typeof filters.limit === "number")
        query.limit = String(filters.limit);

      const { data, error } = await client.get<{ 200: ListAssetsResponse }>({
        url: `${base(siteId)}/assets`,
        query,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
  });
}

/** Start a library sync (operator+) — enumerates WP media into the CP. */
export function useSyncMedia(
  siteId: string,
): UseMutationResult<SyncResponse, Error, void> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { data, error } = await client.post<{ 202: SyncResponse }>({
        url: `${base(siteId)}/sync`,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: [...mediaKeys.all, "assets", siteId] });
    },
  });
}

/** Start an optimize batch (operator+). */
export function useOptimizeMedia(
  siteId: string,
): UseMutationResult<BatchResponse, Error, OptimizeBody> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: OptimizeBody) => {
      const { data, error } = await client.post<{ 202: BatchResponse }>({
        url: `${base(siteId)}/optimize`,
        body,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: [...mediaKeys.all, "assets", siteId] });
      void qc.invalidateQueries({ queryKey: [...mediaKeys.all, "jobs", siteId] });
    },
  });
}

/** Start a restore batch (operator+). */
export function useRestoreMedia(
  siteId: string,
): UseMutationResult<BatchResponse, Error, AssetSelectionBody> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: AssetSelectionBody) => {
      const { data, error } = await client.post<{ 202: BatchResponse }>({
        url: `${base(siteId)}/restore`,
        body,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: [...mediaKeys.all, "assets", siteId] });
      void qc.invalidateQueries({ queryKey: [...mediaKeys.all, "jobs", siteId] });
    },
  });
}

/** Delete originals (admin+, IRREVERSIBLE). */
export function useDeleteOriginals(
  siteId: string,
): UseMutationResult<BatchResponse, Error, AssetSelectionBody> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: AssetSelectionBody) => {
      const { data, error } = await client.post<{ 202: BatchResponse }>({
        url: `${base(siteId)}/delete-originals`,
        body,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: [...mediaKeys.all, "assets", siteId] });
      void qc.invalidateQueries({ queryKey: [...mediaKeys.all, "jobs", siteId] });
    },
  });
}

/** Cancel all non-terminal jobs for a site (operator+). */
export function useCancelMedia(
  siteId: string,
): UseMutationResult<CancelResponse, Error, void> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { data, error } = await client.post<{ 200: CancelResponse }>({
        url: `${base(siteId)}/cancel`,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: [...mediaKeys.all, "jobs", siteId] });
      void qc.invalidateQueries({ queryKey: [...mediaKeys.all, "assets", siteId] });
    },
  });
}
