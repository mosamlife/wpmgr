import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  listMyIdentities,
  unlinkMyIdentity,
  setMyInitialPassword,
  type ConnectedAccounts,
} from "@wpmgr/api";

import { toast } from "@/components/toast";
import { toError, authKeys } from "@/features/auth/use-auth";

// The connected accounts card at settings/security: which sign-in methods this
// account has, removing one, and adding the password a social-only account is
// missing.
//
// Unlike the /auth/2fa/* hooks next door, these go through the generated SDK
// rather than raw `client.get({ url })`, because these three routes are in
// packages/openapi/openapi.yaml and therefore have generated types. Hand-rolled
// shapes here would drift from the contract silently.

export const connectedAccountKeys = {
  identities: ["auth", "identities"] as const,
};

/** The sign-in methods on the caller's own account. */
export function useConnectedAccounts() {
  return useQuery({
    queryKey: connectedAccountKeys.identities,
    queryFn: async (): Promise<ConnectedAccounts> => {
      const { data, error } = await listMyIdentities();
      if (error !== undefined) throw toError(error);
      return data;
    },
    staleTime: 30_000,
    retry: 1,
  });
}

/**
 * Disconnect one provider.
 *
 * THE 409 IS NOT A FAILURE TO SMOOTH OVER. It is the server refusing to leave
 * the account with no way to sign in, and its message names the next step (add
 * a password first), so it is surfaced verbatim rather than replaced with a
 * generic "could not disconnect". The card hides the button in this state
 * already; this is the backstop for a stale list, a second tab, or a request
 * made outside the UI.
 */
export function useUnlinkIdentity() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (provider: string): Promise<void> => {
      const { error, response } = await unlinkMyIdentity({ path: { provider } });
      if (response?.status === 409) {
        const raw = error as { message?: unknown } | null | undefined;
        throw new Error(
          typeof raw?.message === "string"
            ? raw.message
            : "That is the only way you can sign in. Set a password first.",
        );
      }
      if (error !== undefined) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: connectedAccountKeys.identities });
      // has_password and the identity list feed the account's sign-in state,
      // which /auth/me also reports on.
      void queryClient.invalidateQueries({ queryKey: authKeys.me });
      toast.success("Sign-in method disconnected");
    },
  });
}

/** Add a first password to an account that has none. */
export function useSetInitialPassword() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (password: string): Promise<void> => {
      const { error, response } = await setMyInitialPassword({ body: { password } });
      if (response?.status === 409) {
        // Only reachable from a stale page: this account already had a
        // password, so the change form (which asks for the current one) is the
        // right surface.
        throw new Error(
          "This account already has a password. Use the change password form instead.",
        );
      }
      if (error !== undefined) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: connectedAccountKeys.identities });
      void queryClient.invalidateQueries({ queryKey: authKeys.me });
      toast.success("Password set");
    },
  });
}
