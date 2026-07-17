import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import {
  listTags,
  createTag,
  updateTag,
  deleteTag,
  bulkApplyTags,
  type SiteTag,
  type SiteTagCreate,
  type SiteTagUpdate,
  type BulkTagApplyRequest,
  type BulkResultList,
  type ApiError,
} from "@wpmgr/api";

import { sitesKeys } from "@/features/sites/use-sites";

// GH #230 "rich tags" — the tenant-level tag registry (m100). Server-state
// hooks following the same pattern as features/sites/use-sites.ts: unwrap
// `{ data, error, response }`, throw a normalized Error on failure, and let
// TanStack Query own loading/error/success.

export const tagsKeys = {
  all: ["tags"] as const,
  lists: () => [...tagsKeys.all, "list"] as const,
};

/** A 409 on create/rename surfaced as a typed error for inline conflict UI. */
export class TagNameExistsError extends Error {
  constructor(message = "A tag with this name already exists.") {
    super(message);
    this.name = "TagNameExistsError";
  }
}

/** A 404 on update/delete surfaced as a typed error. */
export class TagNotFoundError extends Error {
  constructor(message = "Tag not found") {
    super(message);
    this.name = "TagNotFoundError";
  }
}

/** List the tenant's tag registry — every tag, including unused (usage_count 0). */
export function useTags(): UseQueryResult<SiteTag[], Error> {
  return useQuery({
    queryKey: tagsKeys.lists(),
    queryFn: async () => {
      const { data, error } = await listTags();
      if (error) throw toError(error);
      return data?.items ?? [];
    },
  });
}

/** Create a new tag in the registry. */
export function useCreateTag(): UseMutationResult<
  SiteTag,
  Error,
  SiteTagCreate
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: SiteTagCreate) => {
      const { data, error, response } = await createTag({ body });
      if (response?.status === 409) throw new TagNameExistsError();
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: tagsKeys.lists() });
    },
  });
}

/** Rename, recolor, or merge a tag. */
export function useUpdateTag(): UseMutationResult<
  SiteTag,
  Error,
  { tagId: string; body: SiteTagUpdate }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ tagId, body }) => {
      const { data, error, response } = await updateTag({
        path: { tagId },
        body,
      });
      if (response?.status === 404) throw new TagNotFoundError();
      if (response?.status === 409) throw new TagNameExistsError();
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: tagsKeys.lists() });
      void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
    },
  });
}

/** Delete a tag fleet-wide (registry + every site carrying it). */
export function useDeleteTag(): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (tagId: string) => {
      const { error, response } = await deleteTag({ path: { tagId } });
      if (response?.status === 404) throw new TagNotFoundError();
      if (error) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: tagsKeys.lists() });
      void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
    },
  });
}

/** Apply add/remove tag deltas across many sites in one call. */
export function useBulkApplyTags(): UseMutationResult<
  BulkResultList,
  Error,
  BulkTagApplyRequest
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: BulkTagApplyRequest) => {
      const { data, error } = await bulkApplyTags({ body });
      if (error) throw toError(error);
      return data ?? { results: [] };
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: tagsKeys.lists() });
      void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
    },
  });
}

/** Normalize the generated `Error` body (or anything) into an Error instance. */
function toError(error: unknown): Error {
  if (error instanceof Error) return error;
  if (isApiError(error)) return new Error(error.message);
  return new Error("Request failed");
}

function isApiError(value: unknown): value is ApiError {
  return (
    typeof value === "object" &&
    value !== null &&
    "message" in value &&
    typeof value.message === "string"
  );
}
