import {
  useQuery,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";
import { toast } from "@/components/toast";
import { toError, authKeys } from "@/features/auth/use-auth";
import type { Me } from "@wpmgr/api";

// ---------------------------------------------------------------------------
// Types — all hand-rolled against the /auth/2fa/* contract (not in the
// generated SDK since these endpoints live on the root auth engine, not the
// OpenAPI spec).
// ---------------------------------------------------------------------------

export interface TwoFactorStatus {
  totp_enabled: boolean;
  webauthn_count: number;
  recovery_codes_remaining: number;
  two_factor_enabled: boolean;
  trusted_devices: TrustedDevice[];
}

export interface TrustedDevice {
  id: string;
  label: string;
  user_agent: string;
  created_at: string;
  expires_at: string;
  last_used_at: string;
  ip: string;
}

export interface WebAuthnCredential {
  id: string;
  name: string;
  created_at: string;
  last_used_at: string;
}

export interface TwoFaChallengeResponse {
  two_factor_required: true;
  challenge: string;
  factors: {
    totp: boolean;
    webauthn: boolean;
    recovery: boolean;
  };
}

export interface TotpBeginResult {
  otpauth_uri: string;
  secret: string;
}

export interface ChallengeCompleteResult {
  me: Me;
  recovery_codes_remaining: number;
}

// ---------------------------------------------------------------------------
// Query keys
// ---------------------------------------------------------------------------

export const twoFaKeys = {
  status: ["auth", "2fa", "status"] as const,
  credentials: ["auth", "2fa", "webauthn-credentials"] as const,
  trustedDevices: ["auth", "2fa", "trusted-devices"] as const,
};

// ---------------------------------------------------------------------------
// Management queries (authenticated)
// ---------------------------------------------------------------------------

export function useTwoFaStatus() {
  return useQuery({
    queryKey: twoFaKeys.status,
    queryFn: async (): Promise<TwoFactorStatus> => {
      const result = await client.get({
        url: "/auth/2fa/status",
      });
      if (result.error !== undefined) throw toError(result.error);
      return result.data as TwoFactorStatus;
    },
    staleTime: 30_000,
    retry: 1,
  });
}

export function useWebAuthnCredentials() {
  return useQuery({
    queryKey: twoFaKeys.credentials,
    queryFn: async (): Promise<WebAuthnCredential[]> => {
      const result = await client.get({
        url: "/auth/2fa/webauthn/credentials",
      });
      if (result.error !== undefined) throw toError(result.error);
      const raw = result.data as { items: WebAuthnCredential[] };
      return raw?.items ?? [];
    },
    staleTime: 30_000,
    retry: 1,
  });
}

export function useTrustedDevices() {
  return useQuery({
    queryKey: twoFaKeys.trustedDevices,
    queryFn: async (): Promise<TrustedDevice[]> => {
      const result = await client.get({
        url: "/auth/2fa/trusted-devices",
      });
      if (result.error !== undefined) throw toError(result.error);
      const raw = result.data as { items: TrustedDevice[] };
      return raw?.items ?? [];
    },
    staleTime: 30_000,
    retry: 1,
  });
}

// ---------------------------------------------------------------------------
// TOTP enrollment mutations
// ---------------------------------------------------------------------------

export function useTotpBegin() {
  return useMutation({
    mutationFn: async (): Promise<TotpBeginResult> => {
      const result = await client.post({
        url: "/auth/2fa/totp/begin",
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
      return result.data as TotpBeginResult;
    },
  });
}

export function useTotpConfirm() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: { code: string }): Promise<{ recovery_codes: string[] }> => {
      const result = await client.post({
        url: "/auth/2fa/totp/confirm",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.response?.status === 401) {
        const raw = result.error as Record<string, unknown> | null | undefined;
        const code = raw && typeof raw["code"] === "string" ? raw["code"] : "";
        if (code === "totp_provisional_expired") {
          throw new Error("Setup session expired. Start enrollment again.");
        }
        throw new Error("Invalid code. Check your authenticator app and try again.");
      }
      if (result.response?.status === 410) {
        throw new Error("Setup session expired. Start enrollment again.");
      }
      if (result.error !== undefined) throw toError(result.error);
      return result.data as { recovery_codes: string[] };
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: twoFaKeys.status });
      void queryClient.invalidateQueries({ queryKey: authKeys.me });
    },
  });
}

export function useTotpDisable() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: { current_password: string }): Promise<void> => {
      const result = await client.post({
        url: "/auth/2fa/totp/disable",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.response?.status === 401) {
        throw new Error("Incorrect password.");
      }
      if (result.response?.status === 400) {
        throw new Error("This account uses single sign-on and has no password.");
      }
      if (result.error !== undefined) throw toError(result.error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: twoFaKeys.status });
      void queryClient.invalidateQueries({ queryKey: authKeys.me });
      toast.success("Authenticator app disabled");
    },
  });
}

// ---------------------------------------------------------------------------
// Recovery codes
// ---------------------------------------------------------------------------

export function useRegenerateRecoveryCodes() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: { current_password: string }): Promise<{ recovery_codes: string[] }> => {
      const result = await client.post({
        url: "/auth/2fa/recovery-codes/regenerate",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.response?.status === 401) {
        throw new Error("Incorrect password.");
      }
      if (result.error !== undefined) throw toError(result.error);
      return result.data as { recovery_codes: string[] };
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: twoFaKeys.status });
    },
  });
}

// ---------------------------------------------------------------------------
// WebAuthn registration mutations
// ---------------------------------------------------------------------------

export function useWebAuthnBeginRegistration() {
  return useMutation({
    mutationFn: async (): Promise<Record<string, unknown>> => {
      const result = await client.post({
        url: "/auth/2fa/webauthn/begin-registration",
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
      return result.data as Record<string, unknown>;
    },
  });
}

export function useWebAuthnFinishRegistration() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: { name: string; attestation: string }): Promise<WebAuthnCredential> => {
      const result = await client.post({
        url: "/auth/2fa/webauthn/finish-registration",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.response?.status === 400) {
        throw new Error("Passkey registration failed. Try again.");
      }
      if (result.response?.status === 410) {
        throw new Error("Registration session expired. Start again.");
      }
      if (result.error !== undefined) throw toError(result.error);
      return result.data as WebAuthnCredential;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: twoFaKeys.credentials });
      void queryClient.invalidateQueries({ queryKey: twoFaKeys.status });
      void queryClient.invalidateQueries({ queryKey: authKeys.me });
      toast.success("Passkey added");
    },
  });
}

export function useDeleteWebAuthnCredential() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (params: { id: string; current_password: string }): Promise<void> => {
      const result = await client.delete({
        url: `/auth/2fa/webauthn/credentials/${params.id}`,
        body: { current_password: params.current_password },
        headers: { "Content-Type": "application/json" },
      });
      if (result.response?.status === 401) {
        throw new Error("Incorrect password.");
      }
      if (result.response?.status === 404) {
        throw new Error("Passkey not found.");
      }
      if (result.error !== undefined) throw toError(result.error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: twoFaKeys.credentials });
      void queryClient.invalidateQueries({ queryKey: twoFaKeys.status });
      void queryClient.invalidateQueries({ queryKey: authKeys.me });
      toast.success("Passkey removed");
    },
  });
}

// ---------------------------------------------------------------------------
// Trusted device mutations
// ---------------------------------------------------------------------------

export function useRevokeTrustedDevice() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (id: string): Promise<void> => {
      const result = await client.delete({
        url: `/auth/2fa/trusted-devices/${id}`,
      });
      if (result.error !== undefined) throw toError(result.error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: twoFaKeys.trustedDevices });
      void queryClient.invalidateQueries({ queryKey: twoFaKeys.status });
    },
  });
}

export function useRevokeAllTrustedDevices() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (): Promise<void> => {
      const result = await client.post({
        url: "/auth/2fa/trusted-devices/revoke-all",
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: twoFaKeys.trustedDevices });
      void queryClient.invalidateQueries({ queryKey: twoFaKeys.status });
      toast.success("All trusted devices revoked");
    },
  });
}

// ---------------------------------------------------------------------------
// Challenge completion mutations (unauthenticated — login flow)
// These return the Me object on success; the caller seeds the query cache.
// ---------------------------------------------------------------------------

export function useTotpChallenge() {
  return useMutation({
    mutationFn: async (body: {
      challenge: string;
      code: string;
      remember_device: boolean;
      device_label?: string;
    }): Promise<ChallengeCompleteResult> => {
      const result = await client.post({
        url: "/auth/2fa/totp",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.response?.status === 410) {
        const raw = result.error as Record<string, unknown> | null | undefined;
        const code = raw && typeof raw["code"] === "string" ? raw["code"] : "";
        if (code === "challenge_expired") {
          throw Object.assign(new Error("challenge_expired"), { code: "challenge_expired" });
        }
      }
      if (result.response?.status === 401) {
        const raw = result.error as Record<string, unknown> | null | undefined;
        const code = raw && typeof raw["code"] === "string" ? raw["code"] : "";
        if (code === "too_many_attempts") {
          throw Object.assign(new Error("too_many_attempts"), { code: "too_many_attempts" });
        }
        throw Object.assign(new Error("invalid_code"), { code: "invalid_code" });
      }
      // A 500 with code `totp_decrypt_failed` means the server can no longer
      // decrypt the operator's stored TOTP secret (the secrets-at-rest key
      // changed on a self-host, e.g. GH #215) -- a server configuration
      // problem, not a wrong code. The generic `toError` fallback below would
      // otherwise surface this as an indistinguishable "Verification failed"
      // banner, so it must be branched out here like the 401/410 cases above.
      if (result.response?.status === 500) {
        const raw = result.error as Record<string, unknown> | null | undefined;
        const code = raw && typeof raw["code"] === "string" ? raw["code"] : "";
        if (code === "totp_decrypt_failed") {
          throw Object.assign(new Error("totp_decrypt_failed"), { code: "totp_decrypt_failed" });
        }
      }
      if (result.error !== undefined) throw toError(result.error);
      return result.data as ChallengeCompleteResult;
    },
  });
}

export function useRecoveryChallenge() {
  return useMutation({
    mutationFn: async (body: {
      challenge: string;
      code: string;
      remember_device: boolean;
    }): Promise<ChallengeCompleteResult> => {
      const result = await client.post({
        url: "/auth/2fa/recovery",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.response?.status === 410) {
        const raw = result.error as Record<string, unknown> | null | undefined;
        const code = raw && typeof raw["code"] === "string" ? raw["code"] : "";
        if (code === "challenge_expired") {
          throw Object.assign(new Error("challenge_expired"), { code: "challenge_expired" });
        }
        if (code === "codes_exhausted") {
          throw Object.assign(new Error("codes_exhausted"), { code: "codes_exhausted" });
        }
        if (code === "recovery_code_already_used") {
          throw Object.assign(new Error("recovery_code_already_used"), { code: "recovery_code_already_used" });
        }
      }
      if (result.response?.status === 401) {
        throw Object.assign(new Error("invalid_code"), { code: "invalid_code" });
      }
      if (result.error !== undefined) throw toError(result.error);
      return result.data as ChallengeCompleteResult;
    },
  });
}

export function useWebAuthnBeginChallenge() {
  return useMutation({
    mutationFn: async (body: { challenge: string }): Promise<Record<string, unknown>> => {
      const result = await client.post({
        url: "/auth/2fa/webauthn/begin",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
      return result.data as Record<string, unknown>;
    },
  });
}

export function useWebAuthnFinishChallenge() {
  return useMutation({
    mutationFn: async (body: {
      challenge: string;
      assertion: string;
      remember_device: boolean;
      device_label?: string;
    }): Promise<Me> => {
      const result = await client.post({
        url: "/auth/2fa/webauthn/finish",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.response?.status === 401) {
        const raw = result.error as Record<string, unknown> | null | undefined;
        const code = raw && typeof raw["code"] === "string" ? raw["code"] : "";
        if (code === "cloned_authenticator") {
          throw new Error("Security key may be cloned. Contact support.");
        }
        throw new Error("Passkey verification failed. Try again.");
      }
      if (result.error !== undefined) throw toError(result.error);
      return result.data as Me;
    },
  });
}
