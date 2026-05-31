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

// Members management hooks — hand-rolled endpoints.
// GET /api/v1/members (list)
// PATCH /api/v1/members/{userId} {role} (role change)
// DELETE /api/v1/members/{userId} (remove)
// POST /api/v1/members (invite)

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

export type MemberRole = "owner" | "admin" | "operator" | "viewer";

export interface Member {
  user_id: string;
  tenant_id: string;
  role: MemberRole;
  email?: string;
  name?: string;
  created_at?: string;
}

export interface MemberList {
  items: Member[];
}

export interface InviteMemberResult {
  /** Present when the invitee is a new user — the one-time accept link. */
  accept_link?: string;
}

// ---------------------------------------------------------------------------
// Cache key family
// ---------------------------------------------------------------------------

export const memberKeys = {
  all: ["members"] as const,
  list: () => ["members", "list"] as const,
};

// ---------------------------------------------------------------------------
// useMembers — GET /api/v1/members
// ---------------------------------------------------------------------------

export function useMembers(): UseQueryResult<Member[], Error> {
  return useQuery({
    queryKey: memberKeys.list(),
    queryFn: async () => {
      const result = await client.get({ url: "/api/v1/members" });
      if (result.error !== undefined) throw toError(result.error);
      const data = result.data as MemberList;
      return data.items ?? [];
    },
  });
}

// ---------------------------------------------------------------------------
// useUpdateMemberRole — PATCH /api/v1/members/{userId}
// ---------------------------------------------------------------------------

export function useUpdateMemberRole(): UseMutationResult<
  Member,
  Error,
  { userId: string; role: MemberRole }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ userId, role }) => {
      const result = await client.patch({
        url: `/api/v1/members/${encodeURIComponent(userId)}`,
        body: { role },
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
      return result.data as Member;
    },
    onSuccess: (_updated, { role }) => {
      void queryClient.invalidateQueries({ queryKey: memberKeys.list() });
      toast.success(`Role updated to ${role}`);
    },
    onError: (err) => {
      toast.error(`Could not update role: ${err.message}`);
    },
  });
}

// ---------------------------------------------------------------------------
// useRemoveMember — DELETE /api/v1/members/{userId}
// 4xx = last-owner protection; surface the message.
// ---------------------------------------------------------------------------

export function useRemoveMember(): UseMutationResult<
  void,
  Error,
  string
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (userId: string) => {
      const result = await client.delete({
        url: `/api/v1/members/${encodeURIComponent(userId)}`,
      });
      if (result.error !== undefined) throw toError(result.error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: memberKeys.list() });
      toast.success("Member removed");
    },
    onError: (err) => {
      toast.error(err.message);
    },
  });
}

// ---------------------------------------------------------------------------
// useInviteMember — POST /api/v1/members
// Returns accept_link when the invitee is new.
// ---------------------------------------------------------------------------

export function useInviteMember(): UseMutationResult<
  InviteMemberResult,
  Error,
  { email: string; name?: string; password?: string; role: MemberRole }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const result = await client.post({
        url: "/api/v1/members",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
      return (result.data ?? {}) as InviteMemberResult;
    },
    onSuccess: (_data) => {
      void queryClient.invalidateQueries({ queryKey: memberKeys.list() });
    },
    onError: (err) => {
      toast.error(err.message);
    },
  });
}
