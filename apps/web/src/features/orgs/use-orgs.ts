import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";
import { resetSiteStream } from "@/features/sites/use-site-events";
import { toast } from "@/components/toast";

// Org (tenant) management hooks — hand-rolled endpoints, not in @wpmgr/api.
// Pattern mirrors use-restores.ts / use-auth.ts.

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

export type OrgRole = "owner" | "admin" | "operator" | "viewer";

export interface Org {
  id: string;
  name: string;
  slug: string;
  role: OrgRole;
}

export interface OrgCreated {
  id: string;
  name: string;
  slug: string;
}

export interface ActivateOrgResult {
  active_tenant_id: string;
}

export const orgKeys = {
  all: ["orgs"] as const,
  list: () => ["orgs", "list"] as const,
};

// ---------------------------------------------------------------------------
// GET /api/v1/orgs — the caller's orgs with real names + their role in each.
// ---------------------------------------------------------------------------

export function useOrgs(): UseQueryResult<Org[], Error> {
  return useQuery({
    queryKey: orgKeys.list(),
    queryFn: async () => {
      const result = await client.get({ url: "/api/v1/orgs" });
      if (result.error !== undefined) throw toError(result.error);
      const data = result.data as { items: Org[] };
      return data.items ?? [];
    },
  });
}

// ---------------------------------------------------------------------------
// POST /api/v1/orgs
// ---------------------------------------------------------------------------

export function useCreateOrg(): UseMutationResult<
  OrgCreated,
  Error,
  { name: string; slug?: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const result = await client.post({
        url: "/api/v1/orgs",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
      return result.data as OrgCreated;
    },
    onSuccess: (org) => {
      // Invalidate me so the new org appears in memberships.
      void queryClient.invalidateQueries({ queryKey: ["auth", "me"] });
      toast.success(`Organisation "${org.name}" created`);
    },
  });
}

// ---------------------------------------------------------------------------
// POST /api/v1/orgs/{orgId}/activate
// Switch the session's active org. Clears ALL server state so every query
// refetches under the new org context.
// ---------------------------------------------------------------------------

export function useActivateOrg(): UseMutationResult<
  ActivateOrgResult,
  Error,
  string
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (orgId: string) => {
      const result = await client.post({
        url: `/api/v1/orgs/${encodeURIComponent(orgId)}/activate`,
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
      return result.data as ActivateOrgResult;
    },
    onSuccess: () => {
      // Drop ALL server state — sites, members, me, etc. — so everything
      // refetches in the context of the newly-active org.
      queryClient.clear();
      // The shared Sites SSE stream is a module-level singleton whose tenant
      // is resolved ONCE by the CP at connect time and held for the life of
      // the connection (up to ~15 min). Clearing the cache only fixes the
      // pull (refetch) path — without this, an already-open stream keeps
      // delivering the OLD tenant's events (or none for the new one) until a
      // hard reload or the stream's natural expiry. Reset it so the live
      // (push) path re-establishes under the new tenant too, with a fresh
      // cursor (see resetSiteStream's doc for why the cursor must be dropped).
      resetSiteStream();
    },
    onError: (err) => {
      toast.error(`Could not switch organisation: ${err.message}`);
    },
  });
}

// ---------------------------------------------------------------------------
// PATCH /api/v1/orgs/{orgId} — rename an organisation (admin/owner only).
// ---------------------------------------------------------------------------

export function useRenameOrg(): UseMutationResult<
  OrgCreated,
  Error,
  { orgId: string; name: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ orgId, name }) => {
      const result = await client.patch({
        url: `/api/v1/orgs/${encodeURIComponent(orgId)}`,
        body: { name },
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
      return result.data as OrgCreated;
    },
    onSuccess: (org) => {
      void queryClient.invalidateQueries({ queryKey: orgKeys.list() });
      // me carries memberships used by the switcher; refresh it too.
      void queryClient.invalidateQueries({ queryKey: ["auth", "me"] });
      toast.success(`Organisation renamed to "${org.name}"`);
    },
    onError: (err) => {
      toast.error(`Could not rename organisation: ${err.message}`);
    },
  });
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/orgs/{orgId}: owner-only organisation deletion (GH #152
// part 2). Body: { confirm_name: "<org's current display name, verbatim>" }.
// The server trims outer whitespace only, then requires an EXACT
// case-sensitive match -- never lowercase/normalize the value client-side.
//
// Deletion is SOFT with a grace window: the org disappears from GET /orgs
// the instant this commits (recoverable server-side until a background job
// purges it), so onSuccess mirrors useActivateOrg's cache teardown -- every
// org-scoped query (sites, members, keys, ...) is now stale.
// ---------------------------------------------------------------------------

export interface DeleteOrgResult {
  id: string;
  /** "hard" = the org was empty and removed immediately; "soft" = scheduled for grace-window purge. */
  lane: "hard" | "soft";
}

/** The full set of refusal codes DELETE /orgs/{orgId} is documented to return. */
export type DeleteOrgErrorCode =
  | "confirm_name_required"
  | "confirm_name_mismatch"
  | "cannot_delete_active_org"
  | "billing_active"
  | "restore_in_progress"
  | "not_a_member"
  | "insufficient_role"
  | "org_already_deleted"
  | "org_not_found"
  | "invalid_org_id"
  | "invalid_body";

/**
 * Human, actionable copy for every documented refusal code. Deliberately
 * overrides the server's own wording (aimed at logs/API consumers) with
 * house-style UI copy.
 */
const DELETE_ORG_ERROR_MESSAGES: Record<DeleteOrgErrorCode, string> = {
  confirm_name_required: "Type the organisation's name to confirm deletion.",
  confirm_name_mismatch:
    "That doesn't match the organisation's name. Type it exactly as shown, then try again.",
  cannot_delete_active_org:
    "Switch to another organisation first. You can't delete the one you're currently in.",
  billing_active: "Cancel the subscription before deleting this organisation.",
  restore_in_progress:
    "A restore is running on a site in this organisation. Wait for it to finish, then try again.",
  not_a_member: "You're not a member of this organisation.",
  insufficient_role: "Only the owner can delete this organisation.",
  org_already_deleted: "This organisation is already scheduled for deletion.",
  org_not_found: "This organisation could not be found.",
  invalid_org_id: "This organisation could not be found.",
  invalid_body: "Could not delete organisation: the request was malformed.",
};

function isDeleteOrgErrorCode(code: string): code is DeleteOrgErrorCode {
  return Object.prototype.hasOwnProperty.call(DELETE_ORG_ERROR_MESSAGES, code);
}

/**
 * Maps a DELETE /orgs/{orgId} error code into a clear, human message. Falls
 * back to the server's own message for any undocumented code, so a future
 * backend addition never surfaces as a blank error. Pure + exported so every
 * documented code is covered by use-orgs.test.ts without a network call.
 */
export function mapDeleteOrgError(
  code: string | undefined,
  fallbackMessage: string,
): string {
  if (code && isDeleteOrgErrorCode(code)) {
    return DELETE_ORG_ERROR_MESSAGES[code];
  }
  return fallbackMessage || "Could not delete organisation.";
}

/** Raised by useDeleteOrg; carries the server's error code for callers that need to branch on it. */
export class DeleteOrgError extends Error {
  code?: string;
  constructor(message: string, code?: string) {
    super(message);
    this.name = "DeleteOrgError";
    this.code = code;
  }
}

/**
 * Whether a typed confirmation exactly matches the organisation's name. MUST
 * be an exact, case-sensitive match with no client-side trim/normalize --
 * mirrors the server's confirm_name contract. Exported (pure) so this
 * contract is covered by a test without mounting the Danger Zone dialog.
 */
export function orgDeleteConfirmMatches(typed: string, orgName: string): boolean {
  return typed === orgName;
}

export function useDeleteOrg(): UseMutationResult<
  DeleteOrgResult,
  Error,
  { orgId: string; confirmName: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ orgId, confirmName }) => {
      const result = await client.delete({
        url: `/api/v1/orgs/${encodeURIComponent(orgId)}`,
        body: { confirm_name: confirmName },
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) {
        const raw = result.error as { code?: string; message?: string };
        throw new DeleteOrgError(
          mapDeleteOrgError(raw.code, raw.message ?? "Could not delete organisation."),
          raw.code,
        );
      }
      return result.data as DeleteOrgResult;
    },
    onSuccess: (_result, variables) => {
      // Drop ALL server state, mirroring useActivateOrg: the deleted org
      // disappears from every list server-side immediately, so any cached
      // org-scoped data (sites, members, keys, ...) is now stale.
      queryClient.clear();
      resetSiteStream();
      toast.success(
        `"${variables.confirmName}" is scheduled for permanent deletion`,
        {
          description:
            "It stays recoverable during the grace window before it's purged for good.",
        },
      );
    },
    onError: (err) => {
      toast.error("Could not delete organisation", { description: err.message });
    },
  });
}
