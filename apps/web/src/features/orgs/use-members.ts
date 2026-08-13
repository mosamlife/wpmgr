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
  email: string;
  role: MemberRole;
  /** The tokenized accept link. Always returned so the admin can copy/hand-deliver it. */
  accept_link: string;
}

// ---------------------------------------------------------------------------
// Coded refusals
//
// Follows the `mapDeleteOrgError` idiom in `features/orgs/use-orgs.ts`: a map
// of documented codes to house-style copy, a pure exported mapper that falls
// back to the server's own message for anything undocumented, and an Error
// subclass carrying the code for callers that need to branch.
//
// Every code below was read out of the Go handler rather than guessed:
//   apps/api/internal/auth/members_handler.go
//     :215 Forbidden("target_role_exceeds_actor", "you cannot change the role
//          of a member who outranks you")
//     :268 Forbidden("target_role_exceeds_actor", "you cannot remove a member
//          who outranks you")
//     Forbidden("role_grant_exceeds_actor", "you cannot grant a role higher
//          than your own")   (also apps/api/internal/auth/service.go:331 and
//          apps/api/internal/invitation/service.go:96)
//     Forbidden("last_owner", "cannot demote the last owner" / "cannot remove
//          the last owner")
// ---------------------------------------------------------------------------

/** The refusal codes the members endpoints are documented to return. */
export type MemberErrorCode =
  | "target_role_exceeds_actor"
  | "role_grant_exceeds_actor"
  | "last_owner";

const MEMBER_ERROR_MESSAGES: Record<MemberErrorCode, string> = {
  target_role_exceeds_actor:
    "That member outranks you, so you can't change or remove them. Ask an owner.",
  role_grant_exceeds_actor:
    "You can't grant a role higher than your own. Ask an owner.",
  last_owner:
    "This is the last owner. Promote another member to owner first, then try again.",
};

function isMemberErrorCode(code: string): code is MemberErrorCode {
  return Object.prototype.hasOwnProperty.call(MEMBER_ERROR_MESSAGES, code);
}

/**
 * Maps a members-endpoint refusal code into clear, human copy. Falls back to
 * the server's own message for any undocumented code, so a future backend
 * addition never surfaces as a blank error. Pure + exported so every
 * documented code is covered by a test without a network call.
 */
export function mapMemberError(
  code: string | undefined,
  fallbackMessage: string,
): string {
  if (code && isMemberErrorCode(code)) {
    return MEMBER_ERROR_MESSAGES[code];
  }
  return fallbackMessage || "Request failed";
}

/** Raised by the member mutations; carries the server's refusal code. */
export class MemberError extends Error {
  code?: string;
  constructor(message: string, code?: string) {
    super(message);
    this.name = "MemberError";
    this.code = code;
  }
}

/**
 * Builds the toast copy for a failed member mutation.
 *
 * A documented refusal already reads as a whole sentence about this exact
 * operation ("That member outranks you, so you can't change or remove them"),
 * so prefixing it would double the clause. Everything else is the opposite
 * problem: the generated client throws a raw string for a non-JSON error body
 * (`generated/client/client.gen.ts:202`, an nginx 502 page) and `{}` for an
 * empty one (:216). Neither satisfies `isApiError`, so `toError` flattens both
 * to "Request failed", and a network drop arrives as "Failed to fetch" -- three
 * toasts that never say which operation failed. Those get the operation name.
 */
function withOperationContext(err: Error, operation: string): string {
  const coded =
    err instanceof MemberError &&
    err.code !== undefined &&
    isMemberErrorCode(err.code);
  return coded ? err.message : `${operation}: ${err.message}`;
}

function toMemberError(error: unknown, fallback: string): Error {
  const raw = error as { code?: string; message?: string } | null | undefined;
  const code = typeof raw?.code === "string" ? raw.code : undefined;
  if (code && isMemberErrorCode(code)) {
    return new MemberError(mapMemberError(code, raw?.message ?? fallback), code);
  }
  const base = toError(error);
  return new MemberError(base.message || fallback, code);
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
      if (result.error !== undefined) {
        throw toMemberError(result.error, "Could not update role");
      }
      return result.data as Member;
    },
    onSuccess: (_updated, { role }) => {
      void queryClient.invalidateQueries({ queryKey: memberKeys.list() });
      toast.success(`Role updated to ${role}`);
    },
    onError: (err) => {
      toast.error(withOperationContext(err, "Could not update role"));
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
      if (result.error !== undefined) {
        throw toMemberError(result.error, "Could not remove member");
      }
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: memberKeys.list() });
      toast.success("Member removed");
    },
    onError: (err) => {
      toast.error(withOperationContext(err, "Could not remove member"));
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
  { email: string; role: MemberRole }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const result = await client.post({
        url: "/api/v1/members",
        body,
        headers: { "Content-Type": "application/json" },
      });
      // Invite goes through the same ceiling check
      // (apps/api/internal/invitation/service.go:96 returns
      // role_grant_exceeds_actor), so it maps the same way.
      if (result.error !== undefined) {
        throw toMemberError(result.error, "Could not send invitation");
      }
      return result.data as InviteMemberResult;
    },
    onSuccess: (_data) => {
      void queryClient.invalidateQueries({ queryKey: memberKeys.list() });
    },
    onError: (err) => {
      toast.error(withOperationContext(err, "Could not send invitation"));
    },
  });
}
