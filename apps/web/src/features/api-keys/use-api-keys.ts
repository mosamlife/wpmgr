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
      if (error) throw toError(error);
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
