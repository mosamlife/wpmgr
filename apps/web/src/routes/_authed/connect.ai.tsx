import { createFileRoute } from "@tanstack/react-router";
import { useMemo } from "react";
import { z } from "zod";

import { PageError } from "@/components/feedback/page-error";
import { DEFAULT_SITES_LIMIT, useSites } from "@/features/sites/use-sites";
import { useTags } from "@/features/tags/use-tags";
import {
  ConsentScreen,
  ConsentScreenSkeleton,
} from "@/features/mcp-consent/consent-screen";
import {
  buildDenialTarget,
  buildRedirectTarget,
  navigateTo,
  useApproveConsent,
  useConsentContext,
  type AuthorizeParams,
} from "@/features/mcp-consent/use-consent";
import {
  snapshotFromPage,
  type FleetSnapshot,
  type ScopedSite,
} from "@/features/mcp-consent/site-scope";

// /connect/ai — the browser step of the MCP OAuth flow (ADR-064 S6b, design
// Step 7).
//
// ROUTE PLACEMENT. Under _authed/ deliberately. The API's authorize endpoint
// answers 401 without a principal (apps/api/internal/mcp/handler.go), and a
// consent screen that renders for a logged-out user would show an approve
// button that can only ever fail. The pathless layout's beforeLoad sends them
// to /login first and back here after.
//
// NO SIDEBAR ENTRY. This is a redirect target reached from an external client,
// never navigated to from inside the app, so it stays out of
// components/layout/sidebar.tsx along with the other callback and redirect
// routes.

// Only `response_type`, `client_id`, `redirect_uri` and `scope` are required to
// even ask the question. They are optional in the schema so that a malformed
// launch renders an explanation rather than a router-level parse failure the
// user cannot read -- but a missing one is refused below, not defaulted.
const searchSchema = z.object({
  response_type: z.string().optional(),
  client_id: z.string().optional(),
  redirect_uri: z.string().optional(),
  scope: z.string().optional(),
  state: z.string().optional(),
  code_challenge: z.string().optional(),
  code_challenge_method: z.string().optional(),
});

export const Route = createFileRoute("/_authed/connect/ai")({
  validateSearch: searchSchema,
  component: ConnectAiPage,
});

function ConnectAiPage() {
  const search = Route.useSearch();

  // An INCOMPLETE REQUEST IS NOT A REQUEST. No parameter is filled in with a
  // plausible default -- in particular `scope` is never defaulted, because
  // ParseRequestedScopes refuses an absent scope rather than granting one, and
  // a screen that quietly supplied "mcp:read" would be consenting on the
  // client's behalf to something it never asked for.
  const params: AuthorizeParams | null = useMemo(() => {
    if (!search.client_id || !search.redirect_uri || !search.scope) return null;
    return {
      response_type: search.response_type ?? "code",
      client_id: search.client_id,
      redirect_uri: search.redirect_uri,
      scope: search.scope,
      state: search.state,
      code_challenge: search.code_challenge,
      code_challenge_method: search.code_challenge_method,
    };
  }, [search]);

  const consentQuery = useConsentContext(params);
  const sitesQuery = useSites({ view: "active" });
  const tagsQuery = useTags();
  const approve = useApproveConsent();

  // NULL, NOT [], WHEN WE CANNOT SEE THE FLEET. resolveSiteScope treats the two
  // differently on purpose: an empty snapshot is a fleet with no sites, and
  // null is a fleet we have not read. Collapsing them would let a failed site
  // load render as a confident, approvable "0 sites".
  //
  // AND A FULL PAGE IS NOT A WHOLE FLEET. listSites is paged and SiteList is
  // `{ items }` with no total and no has_more, so the only truthful reading of
  // a full page is "at least this many". snapshotFromPage carries that fact
  // forward instead of letting the screen treat the page as the fleet -- the
  // partial-rendered-as-complete defect, on the screen where it costs consent.
  const fleet: FleetSnapshot | null = useMemo(() => {
    if (sitesQuery.data === undefined) return null;
    const sites: ScopedSite[] = sitesQuery.data.map((s) => ({
      id: s.id,
      name: s.name,
      url: s.url,
    }));
    return snapshotFromPage(sites, DEFAULT_SITES_LIMIT);
  }, [sitesQuery.data]);

  const tagsBySiteId = useMemo(() => {
    const out: Record<string, readonly string[]> = {};
    for (const site of sitesQuery.data ?? []) out[site.id] = site.tags ?? [];
    return out;
  }, [sitesQuery.data]);

  const tags = useMemo(
    () => (tagsQuery.data ?? []).map((t) => ({ id: t.id, name: t.name })),
    [tagsQuery.data],
  );

  if (params === null) {
    return (
      <div className="mx-auto max-w-2xl p-4 sm:p-6">
        <PageError
          what="This connection request is incomplete."
          why="It arrived without the details we need to tell you who is asking or what they want to read. Start the connection again from the app you are connecting, and if it keeps failing, that app is sending an incomplete request."
        />
      </div>
    );
  }

  if (consentQuery.isPending) return <ConsentScreenSkeleton />;

  // A FAILED LOAD RENDERS NO APPROVE CONTROL. This branch returns before
  // ConsentScreen is ever constructed, so there is no state in which the user
  // is looking at an approvable screen built from a payload we could not read.
  if (consentQuery.isError || consentQuery.data === undefined) {
    return (
      <div className="mx-auto max-w-2xl p-4 sm:p-6">
        <PageError
          what="We could not check this connection request, so there is nothing here to approve."
          why={
            consentQuery.error instanceof Error
              ? consentQuery.error.message
              : "The server did not answer with a request we could read."
          }
          onRetry={() => void consentQuery.refetch()}
          isRetrying={consentQuery.isFetching}
        />
      </div>
    );
  }

  return (
    <ConsentScreen
      consent={consentQuery.data}
      tags={tags}
      fleet={fleet}
      tagsBySiteId={tagsBySiteId}
      sitesLoading={sitesQuery.isPending}
      isApproving={approve.isPending}
      approveError={approve.error}
      onApprove={(input) => {
        approve.mutate(
          { consent: consentQuery.data, ...input },
          {
            onSuccess: (result) => {
              // Hand control back to the destination the SERVER returned, never
              // to the value that arrived in this browser's query string.
              navigateTo(buildRedirectTarget(result));
            },
          },
        );
      }}
      onDeny={() => {
        // REFUSAL TRAVELS BACK TO THE CLIENT. history.back() left the
        // initiating client waiting for a callback that never came, and did
        // nothing at all when this page was opened in a fresh tab -- which is
        // how an OAuth client normally opens it. RFC 6749 section 4.1.2.1 has
        // a way to say no; use it.
        navigateTo(buildDenialTarget(consentQuery.data));
      }}
    />
  );
}
