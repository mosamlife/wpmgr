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
// Invitation (link-history) types
// ---------------------------------------------------------------------------

export type InvitationStatus = "pending" | "accepted" | "expired" | "revoked";

export interface SiteInvitation {
  id: string;
  site_id?: string;
  email: string;
  role: ShareRole;
  status: InvitationStatus;
  expires_at: string;
  created_at: string;
  accepted_at?: string | null;
  revoked_at?: string | null;
  attempts: number;
  invited_by?: string | null;
}

// ---------------------------------------------------------------------------
// Cache key families
// ---------------------------------------------------------------------------

export const shareKeys = {
  all: ["shares"] as const,
  forSite: (siteId: string) => ["shares", "site", siteId] as const,
};

export const inviteKeys = {
  all: ["invitations"] as const,
  forSite: (siteId: string) => ["invitations", "site", siteId] as const,
};

// statusError maps an empty-bodied error response to human copy. API error
// bodies are empty over the wire (a known session-middleware issue), so we
// branch on the HTTP status; the `message` read is a no-op today and a bonus
// once the body is restored.
function statusError(
  raw: Response,
  json: Record<string, unknown>,
  fallbackByStatus: Record<number, string>,
): Error {
  const msg = typeof json["message"] === "string" ? json["message"] : "";
  return new Error(
    msg || fallbackByStatus[raw.status] || `Request failed: ${raw.status}`,
  );
}

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

// ---------------------------------------------------------------------------
// useSiteInvitations — GET /api/v1/sites/{siteId}/invitations  (link history)
// ---------------------------------------------------------------------------

export function useSiteInvitations(
  siteId: string,
): UseQueryResult<SiteInvitation[], Error> {
  return useQuery({
    queryKey: inviteKeys.forSite(siteId),
    queryFn: async () => {
      const result = await client.get({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/invitations`,
      });
      if (result.error !== undefined) throw toError(result.error);
      const data = result.data as { items: SiteInvitation[] };
      return data.items ?? [];
    },
    enabled: Boolean(siteId),
  });
}

// ---------------------------------------------------------------------------
// useRevokeInvitation — DELETE /api/v1/sites/{siteId}/invitations/{invitationId}
// ---------------------------------------------------------------------------

export function useRevokeInvitation(
  siteId: string,
): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (invitationId: string) => {
      const raw = await fetch(
        `/api/v1/sites/${encodeURIComponent(siteId)}/invitations/${encodeURIComponent(invitationId)}`,
        { method: "DELETE", credentials: "include" },
      );
      if (!raw.ok) {
        const json = (await raw.json().catch(() => ({}))) as Record<
          string,
          unknown
        >;
        throw statusError(raw, json, {
          404: "This invite no longer exists.",
          409: "This invite was already accepted, expired, or revoked.",
        });
      }
    },
    onSuccess: () => {
      // Invalidate BOTH families: an invite accepted in a race becomes a share.
      void queryClient.invalidateQueries({
        queryKey: inviteKeys.forSite(siteId),
      });
      void queryClient.invalidateQueries({
        queryKey: shareKeys.forSite(siteId),
      });
      toast.success("Invite cancelled");
    },
    onError: (err) => {
      toast.error(err.message);
    },
  });
}

// ---------------------------------------------------------------------------
// useRegenerateInvite — POST .../invitations/{invitationId}/regenerate
// Rotates the token; returns the fresh one-time accept link.
// ---------------------------------------------------------------------------

export function useRegenerateInvite(
  siteId: string,
): UseMutationResult<string, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (invitationId: string): Promise<string> => {
      const raw = await fetch(
        `/api/v1/sites/${encodeURIComponent(siteId)}/invitations/${encodeURIComponent(invitationId)}/regenerate`,
        { method: "POST", credentials: "include" },
      );
      const json = (await raw.json().catch(() => ({}))) as Record<
        string,
        unknown
      >;
      if (!raw.ok) {
        throw statusError(raw, json, {
          404: "This invite no longer exists.",
          409: "This invite was already accepted — create a new one instead.",
        });
      }
      return (json["accept_link"] as string | undefined) ?? "";
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: inviteKeys.forSite(siteId),
      });
    },
    onError: (err) => {
      toast.error(err.message);
    },
  });
}
