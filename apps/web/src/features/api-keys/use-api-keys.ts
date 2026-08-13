import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  listApiKeys,
  createApiKey,
  revokeApiKey,
  type ApiKey,
  type ApiKeyCreate,
  type ApiKeyCreated,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// Server-state hooks for tenant API keys (admin/owner only — the backend
// enforces the role regardless of the UI gate).

export const apiKeysKeys = {
  all: ["api-keys"] as const,
  list: () => [...apiKeysKeys.all, "list"] as const,
};

// ---------------------------------------------------------------------------
// Coded refusals — same idiom as `mapDeleteOrgError` in
// `features/orgs/use-orgs.ts`.
//
// Read out of the Go handler, not guessed:
//   apps/api/internal/apikey/handler.go:59
//     Forbidden("apikey_role_exceeds_actor",
//               "you cannot create an API key with a role higher than your own")
// ---------------------------------------------------------------------------

/** The refusal codes POST /api/v1/api-keys is documented to return. */
export type ApiKeyErrorCode = "apikey_role_exceeds_actor";

const API_KEY_ERROR_MESSAGES: Record<ApiKeyErrorCode, string> = {
  apikey_role_exceeds_actor:
    "A key can't carry a role higher than your own. Pick a lower role, or ask an owner.",
};

function isApiKeyErrorCode(code: string): code is ApiKeyErrorCode {
  return Object.prototype.hasOwnProperty.call(API_KEY_ERROR_MESSAGES, code);
}

/**
 * Maps an API-key refusal code into clear, human copy. Falls back to the
 * server's own message for any undocumented code. Pure + exported so the
 * documented code is covered by a test without a network call.
 */
export function mapApiKeyError(
  code: string | undefined,
  fallbackMessage: string,
): string {
  if (code && isApiKeyErrorCode(code)) {
    return API_KEY_ERROR_MESSAGES[code];
  }
  return fallbackMessage || "Could not create API key";
}

/** Raised by useCreateApiKey; carries the server's refusal code. */
export class ApiKeyError extends Error {
  code?: string;
  constructor(message: string, code?: string) {
    super(message);
    this.name = "ApiKeyError";
    this.code = code;
  }
}

export function useApiKeys(): UseQueryResult<ApiKey[], Error> {
  return useQuery({
    queryKey: apiKeysKeys.list(),
    queryFn: async () => {
      const { data, error } = await listApiKeys();
      if (error) throw toError(error);
      return data?.items ?? [];
    },
  });
}

export function useCreateApiKey(): UseMutationResult<
  ApiKeyCreated,
  Error,
  ApiKeyCreate
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: ApiKeyCreate) => {
      const { data, error } = await createApiKey({ body });
      if (error) {
        const raw = error as { code?: string; message?: string } | null;
        const code = typeof raw?.code === "string" ? raw.code : undefined;
        throw new ApiKeyError(
          mapApiKeyError(code, toError(error).message || "Could not create API key"),
          code,
        );
      }
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: apiKeysKeys.all });
    },
  });
}

export function useRevokeApiKey(): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (apiKeyId: string) => {
      const { error } = await revokeApiKey({ path: { apiKeyId } });
      if (error) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: apiKeysKeys.all });
    },
  });
}
