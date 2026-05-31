import {
  useMutation,
  useQueryClient,
  type UseMutationResult,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";
import { toast } from "@/components/toast";

// Org (tenant) management hooks — hand-rolled endpoints, not in @wpmgr/api.
// Pattern mirrors use-restores.ts / use-auth.ts.

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

export interface OrgCreated {
  id: string;
  name: string;
  slug: string;
}

export interface ActivateOrgResult {
  active_tenant_id: string;
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
    },
    onError: (err) => {
      toast.error(`Could not switch organisation: ${err.message}`);
    },
  });
}
