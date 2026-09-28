import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toast } from "@/components/toast";
import { toError } from "@/features/auth/use-auth";

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface SmtpSettings {
  enabled: boolean;
  host: string;
  port: number;
  username: string;
  from_address: string;
  from_name: string;
  tls_mode: "starttls" | "tls" | "none";
  allow_insecure_tls: boolean;
  /** Never contains the actual password — indicates whether one is stored. */
  password_set: boolean;
  updated_at: string;
}

export interface PutSmtpBody {
  enabled: boolean;
  host: string;
  port: number;
  username: string;
  from_address: string;
  from_name: string;
  tls_mode: "starttls" | "tls" | "none";
  allow_insecure_tls: boolean;
  /** Omit or send empty string to keep the stored password unchanged. */
  password?: string;
}

export interface SmtpTestResult {
  ok: boolean;
  message: string;
}

// ---------------------------------------------------------------------------
// Query-key factory
// ---------------------------------------------------------------------------

export const smtpKeys = {
  all: ["settings", "smtp"] as const,
  detail: () => ["settings", "smtp", "detail"] as const,
} as const;

// ---------------------------------------------------------------------------
// Hooks
// ---------------------------------------------------------------------------

/**
 * GET /api/v1/settings/smtp. The route requires instance-level authority, so
 * callers that already know the signed-in user lacks
 * `me.can_manage_instance_email` should pass `enabled: false` rather than
 * let this fire a request that can only 403.
 */
export function useSmtp(
  options: { enabled?: boolean } = {},
): UseQueryResult<SmtpSettings, Error> {
  return useQuery({
    queryKey: smtpKeys.detail(),
    queryFn: async () => {
      const result = await client.get({
        url: "/api/v1/settings/smtp",
      });
      if (result.error !== undefined) throw toError(result.error);
      return result.data as SmtpSettings;
    },
    staleTime: 60_000,
    retry: false,
    enabled: options.enabled ?? true,
  });
}

/** PUT /api/v1/settings/smtp — invalidates the GET query on success and shows a toast. */
export function usePutSmtp(): UseMutationResult<SmtpSettings, Error, PutSmtpBody> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: PutSmtpBody) => {
      const result = await client.put({
        url: "/api/v1/settings/smtp",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
      return result.data as SmtpSettings;
    },
    onSuccess: (updated) => {
      // Seed the cache so the page reflects the latest state without a refetch.
      queryClient.setQueryData(smtpKeys.detail(), updated);
      toast.success("SMTP settings saved");
    },
    onError: (err) => {
      toast.error("Could not save SMTP settings", { description: err.message });
    },
  });
}

/** POST /api/v1/settings/smtp/test — sends a test email. */
export function useTestSmtp(): UseMutationResult<
  SmtpTestResult,
  Error,
  { to_address: string }
> {
  return useMutation({
    mutationFn: async (body) => {
      const result = await client.post({
        url: "/api/v1/settings/smtp/test",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
      return result.data as SmtpTestResult;
    },
  });
}
