import { useEffect, useRef, type RefObject } from "react";
import type { InfiniteData } from "@tanstack/react-query";

import { AbilityRequestError, useOrgAbilityRequestPages } from "@/features/ability-requests/use-ability-requests";

import { AssistantRequestError, useAssistantRequestPages } from "./use-ai-requests";

// The link an AI hands a person when a change waits for them is
// `<base>/ai/requests?request=<id>` (design §7, layer 1). This resolves that id
// against the two queues the page lists, so the page can scroll to the card and
// focus it, or say plainly why it cannot.
//
// A queue is paged newest first, so an id may sit beyond the first page. The
// lookup reads further pages, but only up to DEEP_LINK_MAX_PAGES per queue, so
// a link that names nothing ends the search instead of walking an unbounded
// history. Reaching the cap without finding it reads as "not found", which the
// copy hedges ("It may belong to another organisation").

export const DEEP_LINK_MAX_PAGES = 10;

export type DeepLinkState =
  /** The address names no request. */
  | { readonly kind: "none" }
  /** A queue is still being read. */
  | { readonly kind: "searching" }
  /** A card for the request is on the page. */
  | { readonly kind: "found" }
  /** Every queue was read and none holds it. */
  | { readonly kind: "not_found" }
  /** The person may not read the requests that could hold it. */
  | { readonly kind: "no_permission" }
  /** A queue failed to load for another reason; that queue shows its own error. */
  | { readonly kind: "unknown" };

type ListVerdict = "found" | "searching" | "absent" | "forbidden" | "failed";

interface PagedList {
  readonly isPending: boolean;
  readonly isError: boolean;
  readonly error: Error | null;
  readonly data: InfiniteData<{ readonly requests: ReadonlyArray<{ readonly id: string }> }, number> | undefined;
  readonly hasNextPage: boolean;
  readonly isFetchingNextPage: boolean;
  readonly isFetchNextPageError: boolean;
}

function holds(list: PagedList, id: string): boolean {
  return list.data?.pages.some((p) => p.requests.some((r) => r.id === id)) ?? false;
}

function isForbidden(error: Error | null): boolean {
  return (
    (error instanceof AbilityRequestError || error instanceof AssistantRequestError) && error.status === 403
  );
}

function verdict(list: PagedList, id: string): ListVerdict {
  if (holds(list, id)) return "found";
  if (list.data === undefined) {
    if (list.isPending) return "searching";
    return list.isError && isForbidden(list.error) ? "forbidden" : "failed";
  }
  if (list.isFetchNextPageError) return "failed";
  if (list.isFetchingNextPage) return "searching";
  return list.hasNextPage && list.data.pages.length < DEEP_LINK_MAX_PAGES ? "searching" : "absent";
}

export function resolveDeepLink(
  id: string | undefined,
  ability: PagedList,
  cache: PagedList,
): DeepLinkState {
  if (!id) return { kind: "none" };
  const a = verdict(ability, id);
  const c = verdict(cache, id);
  if (a === "found" || c === "found") return { kind: "found" };
  if (a === "searching" || c === "searching") return { kind: "searching" };
  if (a === "failed" || c === "failed") return { kind: "unknown" };
  if (a === "forbidden") return { kind: "no_permission" };
  return { kind: "not_found" };
}

function pageable(list: PagedList, id: string): boolean {
  return (
    list.hasNextPage &&
    !list.isFetchingNextPage &&
    !list.isFetchNextPageError &&
    !holds(list, id) &&
    (list.data?.pages.length ?? 0) < DEEP_LINK_MAX_PAGES
  );
}

/**
 * Resolves the request named in the address across both queues, reading further
 * pages while it is not yet found. The queries are the page's own (same keys),
 * so this adds no requests of its own beyond the extra pages.
 */
export function useDeepLink(id: string | undefined): DeepLinkState {
  const ability = useOrgAbilityRequestPages();
  const cache = useAssistantRequestPages();

  const abilityMore = id !== undefined && pageable(ability, id);
  const cacheMore = id !== undefined && pageable(cache, id);
  const fetchMoreAbility = ability.fetchNextPage;
  const fetchMoreCache = cache.fetchNextPage;
  // Each arriving page re-runs the effect, so the search walks on one page at a
  // time until the request is found, the queue ends or the cap is reached.
  const abilityPages = ability.data?.pages.length ?? 0;
  const cachePages = cache.data?.pages.length ?? 0;

  useEffect(() => {
    if (abilityMore) void fetchMoreAbility();
  }, [abilityMore, abilityPages, fetchMoreAbility]);
  useEffect(() => {
    if (cacheMore) void fetchMoreCache();
  }, [cacheMore, cachePages, fetchMoreCache]);

  return resolveDeepLink(id, ability, cache);
}

/**
 * For the card a link names: on arrival, scroll it into view and put focus on
 * its Decline button, the safe choice, or on the card itself when nothing is
 * left to decide (a request already answered or closed). It acts once per
 * arrival, so a later refresh of the list does not pull the page back.
 */
export function useDeepLinkFocus(
  active: boolean,
  hasDecline: boolean,
): { articleRef: RefObject<HTMLElement | null>; declineRef: RefObject<HTMLButtonElement | null> } {
  const articleRef = useRef<HTMLElement | null>(null);
  const declineRef = useRef<HTMLButtonElement | null>(null);
  const done = useRef(false);

  useEffect(() => {
    if (!active) {
      done.current = false;
      return;
    }
    if (done.current) return;
    const target = hasDecline ? declineRef.current : articleRef.current;
    if (!target) return;
    done.current = true;
    // jsdom has no scrollIntoView; a browser always does.
    articleRef.current?.scrollIntoView?.({ block: "center" });
    target.focus({ preventScroll: true });
  }, [active, hasDecline]);

  return { articleRef, declineRef };
}
