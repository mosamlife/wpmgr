import { useCallback } from "react";
import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  getSiteAutologinPolicy,
  putSiteAutologinPolicy,
  type SiteAutologinPolicy,
  type SiteAutologinPolicyUpdate,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";
import { toast } from "@/components/toast";

// GH #286: TanStack Query hooks for the per-site default "Login As User"
// policy.
//
// GET  /api/v1/sites/:siteId/autologin-policy  -> SiteAutologinPolicy
// PUT  /api/v1/sites/:siteId/autologin-policy  -> SiteAutologinPolicy
//
// Cache key family: ["autologinPolicy", siteId]
// Convention mirrors use-login-brand.ts.
//
// The endpoint is gated on the `site:autologin` permission (owner/admin);
// callers on operator/viewer roles should pass `{ enabled: false }` so the
// query never fires and 403s.

export const autologinPolicyKeys = {
  forSite: (siteId: string) => ["autologinPolicy", siteId] as const,
};

// ---------------------------------------------------------------------------
// useAutologinPolicy: GET /sites/:siteId/autologin-policy
// ---------------------------------------------------------------------------

export function useAutologinPolicy(
  siteId: string,
  options?: { enabled?: boolean },
): UseQueryResult<SiteAutologinPolicy, Error> {
  return useQuery({
    queryKey: autologinPolicyKeys.forSite(siteId),
    queryFn: async () => {
      const { data, error } = await getSiteAutologinPolicy({
        path: { siteId },
      });
      if (error) throw toError(error);
      if (!data) throw new Error("No autologin policy returned");
      return data;
    },
    enabled: options?.enabled ?? true,
    staleTime: 30_000,
  });
}

// ---------------------------------------------------------------------------
// useUpdateAutologinPolicy: PUT /sites/:siteId/autologin-policy
// ---------------------------------------------------------------------------

export function useUpdateAutologinPolicy(
  siteId: string,
): UseMutationResult<SiteAutologinPolicy, Error, SiteAutologinPolicyUpdate> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: SiteAutologinPolicyUpdate) => {
      const { data, error } = await putSiteAutologinPolicy({
        path: { siteId },
        body,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("No autologin policy returned");
      return data;
    },
    onSuccess: (updated) => {
      // Reflect the returned policy immediately, no refetch needed.
      queryClient.setQueryData(autologinPolicyKeys.forSite(siteId), updated);
    },
  });
}

// ---------------------------------------------------------------------------
// useSaveDefaultLoginUser: the "Make this the default for this site" wiring
// shared by the site-detail header's AutoLoginButton and the Settings tab's
// AutoLoginButton (GH #286).
//
// Both surfaces fetch the same policy (shared query key, one network round
// trip) and need the identical save-default callback: read the current
// `enabled` flag, PUT it back unchanged alongside the new default username,
// and toast the outcome. Extracted here so the two route files can't drift.
// ---------------------------------------------------------------------------

export interface UseSaveDefaultLoginUserResult {
  /** The underlying policy query, so callers can read `data`/`isPending`. */
  policy: UseQueryResult<SiteAutologinPolicy, Error>;
  /**
   * Persists `username` as the site's new default, keeping `enabled`
   * unchanged. Non-blocking: callers fire this alongside an autologin
   * request without awaiting it.
   */
  save: (username: string) => void;
}

export function useSaveDefaultLoginUser(
  siteId: string,
  options?: { enabled?: boolean },
): UseSaveDefaultLoginUserResult {
  const policy = useAutologinPolicy(siteId, options);
  const update = useUpdateAutologinPolicy(siteId);

  const save = useCallback(
    (username: string) => {
      const currentEnabled = policy.data?.enabled ?? true;
      update.mutate(
        { enabled: currentEnabled, default_wp_user_login: username },
        {
          onSuccess: () => {
            toast.success("Default login user saved.");
          },
          onError: (err) => {
            toast.error("Couldn't save the default login user.", {
              description: err.message,
            });
          },
        },
      );
    },
    [policy.data?.enabled, update],
  );

  return { policy, save };
}
