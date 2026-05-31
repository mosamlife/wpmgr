import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";
import { toast } from "@/components/toast";

// Per-site sharing hooks — hand-rolled endpoints, not in @wpmgr/api.
// GET  /api/v1/sites/{siteId}/shares
// POST /api/v1/sites/{siteId}/shares
// DELETE /api/v1/sites/{siteId}/shares/{userId}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

export type ShareRole = "viewer" | "operator" | "admin";

export interface SiteShare {
  id: string;
  site_id: string;
  user_id: string;
  email?: string;
  name?: string;
  role: ShareRole;
  expires_at?: string | null;
  created_at: string;
  granted_by?: string | null;
}

export interface CreateShareBody {
  email: string;
  role: ShareRole;
  expires_at?: string | null;
}

/**
 * 201 = existing user, share created immediately (no accept_link).
 * 202 = new user, invitation created; accept_link is returned.
 */
export interface CreateShareResult {
  /** The share (present on 201). */
  share?: SiteShare;
  /** One-time invite link (present on 202 only). */
  accept_link?: string;
  /** HTTP status from the server. */
  status: 201 | 202;
}

// ---------------------------------------------------------------------------
// Cache key family
// ---------------------------------------------------------------------------

export const shareKeys = {
  all: ["shares"] as const,
  forSite: (siteId: string) => ["shares", "site", siteId] as const,
};

// ---------------------------------------------------------------------------
// useSiteShares — GET /api/v1/sites/{siteId}/shares
// ---------------------------------------------------------------------------

export function useSiteShares(
  siteId: string,
): UseQueryResult<SiteShare[], Error> {
  return useQuery({
    queryKey: shareKeys.forSite(siteId),
    queryFn: async () => {
      const result = await client.get({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/shares`,
      });
      if (result.error !== undefined) throw toError(result.error);
      const data = result.data as { items: SiteShare[] };
      return data.items ?? [];
    },
    enabled: Boolean(siteId),
  });
}

// ---------------------------------------------------------------------------
// useCreateShare — POST /api/v1/sites/{siteId}/shares
// ---------------------------------------------------------------------------

export function useCreateShare(
  siteId: string,
): UseMutationResult<CreateShareResult, Error, CreateShareBody> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const raw = await fetch(
        `/api/v1/sites/${encodeURIComponent(siteId)}/shares`,
        {
          method: "POST",
          credentials: "include",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        },
      );
      const json = (await raw.json().catch(() => ({}))) as Record<
        string,
        unknown
      >;
      if (!raw.ok) {
        const msg =
          typeof json["message"] === "string"
            ? json["message"]
            : `Request failed: ${raw.status}`;
        throw new Error(msg);
      }
      const status = raw.status === 202 ? 202 : 201;
      return {
        share: status === 201 ? (json as unknown as SiteShare) : undefined,
        accept_link:
          status === 202
            ? (json["accept_link"] as string | undefined)
            : undefined,
        status,
      } satisfies CreateShareResult;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: shareKeys.forSite(siteId),
      });
    },
    onError: (err) => {
      toast.error(err.message);
    },
  });
}

// ---------------------------------------------------------------------------
// useRevokeShare — DELETE /api/v1/sites/{siteId}/shares/{userId}
// ---------------------------------------------------------------------------

export function useRevokeShare(
  siteId: string,
): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (userId: string) => {
      const result = await client.delete({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/shares/${encodeURIComponent(userId)}`,
      });
      if (result.error !== undefined) throw toError(result.error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: shareKeys.forSite(siteId),
      });
      toast.success("Access revoked");
    },
    onError: (err) => {
      toast.error(err.message);
    },
  });
}
