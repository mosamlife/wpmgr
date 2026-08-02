import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { Archive, Search } from "lucide-react";
import { z } from "zod";

import { useClients } from "@/features/clients/use-clients";
import { SetClientDialog } from "@/features/clients/set-client-dialog";
import { useFleetAgentVersions } from "@/features/fleet/use-fleet-agents";
import { AGENT_STATUS_LABEL, type AgentStatus } from "@/components/status";

import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { FilterEmpty, SitesPageEmpty } from "@/components/empty";
import { PageHeader } from "@/components/shared/page-header";
import {
  useSites,
  useDeleteSite,
  sitesQueryOptions,
  type TagMatchMode,
} from "@/features/sites/use-sites";
import { useTags } from "@/features/tags/use-tags";
import { useTagColorMap } from "@/features/tags/use-tag-color-map";
import { TagChip } from "@/features/sites/tag-chip";
import { SitesTable } from "@/features/sites/sites-table";
import { SitesGrid, SitesGridSkeleton } from "@/features/sites/sites-grid";
import { SitesToolbar } from "@/features/sites/sites-toolbar";
import { useSitesSelection } from "@/features/sites/use-sites-selection";
import { useSitesDensity } from "@/features/sites/use-sites-density";
import { useSitesView, useCardSize } from "@/features/sites/use-sites-view";
import { AddSiteDialog } from "@/features/sites/add-site-dialog";
import { useSitesLiveSync } from "@/features/sites/use-sites-live";
import {
  useRevokeSite,
  useRestoreSite,
  useCreateEnrollmentCode,
} from "@/features/sites/use-site-connection";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { useAutoLogin, canAutoLogin } from "@/features/sites/use-autologin";
import { OpenInAdminPanel } from "@/features/sites/open-in-admin-panel";
import {
  UpdateWizard,
  type WizardTarget,
} from "@/features/updates/update-wizard";
import { AgentSelfUpdateDialog } from "@/features/updates/agent-self-update-dialog";
import { useMe, canOperate, canManage } from "@/features/auth/use-auth";
import { useBulkBackup } from "@/features/backups/use-bulk-backup";
import { useBulkAction } from "@/features/sites/use-bulk-action";
import { toast } from "@/components/toast";
import { cn } from "@/lib/utils";
import { connectionStateOf } from "@/features/sites/connection-state";
import { agentStatusFilterHint } from "@/features/sites/agent-status-hint";
import type { Site } from "@wpmgr/api";

// ---------------------------------------------------------------------------
// Search param schema
//
// All filter axes live in the URL so they persist across reload and are
// shareable. The route writes via `navigate({ search: prev => ({...}) },
// { replace: true })` with a 200-300ms debounce on the free-text query.
//
// CRITICAL INVARIANT: the `selected` set (useSitesSelection) reads the FULL
// `sites` array, NOT `visibleSites`. Filtering out a selected site must keep
// it in the bulk target — users who select 47 sites and filter down to "Down"
// sites should still hit all 47 when they click "Update plugins".
// ---------------------------------------------------------------------------

const searchSchema = z.object({
  q: z.string().optional(),
  status: z.array(z.string()).optional(),
  tags: z.array(z.string()).optional(),
  // GH #230 "rich tags" — match semantics across `tags`: "any" (OR, default)
  // or "all" (AND). Mirrors the /sites `tags_match` query param.
  tagMode: z.enum(["any", "all"]).optional(),
  client: z.string().optional(),
  archived: z.boolean().optional(),
  view: z.enum(["list", "grid"]).optional(),
  // Agent-freshness classification (agent-releases visibility). Stores
  // display labels, matching the `status` axis above; see AGENT_STATUS_LABEL.
  agentStatus: z.array(z.string()).optional(),
});

type SitesSearch = z.infer<typeof searchSchema>;

export const Route = createFileRoute("/_authed/sites/")({
  validateSearch: searchSchema,
  // Fire the default sites list fetch immediately after auth resolves, without
  // awaiting it. This overlaps the network request with React's code-split
  // chunk load and component mount so the page is data-ready sooner.
  //
  // Why not await: we want the skeleton to show instantly and let TanStack
  // Query manage the loading state — stalling the loader here would delay
  // the render without benefit (the component already handles isPending).
  //
  // Scope: this loader runs only for /_authed/sites/ — no other authed route
  // fires this prefetch. The key matches sitesKeys.list("active") exactly
  // (the default, filter-free view), so useSites({ view: "active" }) with no
  // other filters subscribes to the already-in-flight query; TanStack Query
  // deduplicates by key.
  loader: ({ context: { queryClient } }) => {
    void queryClient.prefetchQuery(sitesQueryOptions());
  },
  component: SitesPage,
});

// ---------------------------------------------------------------------------
// Human-readable label for each ConnectionState value
// ---------------------------------------------------------------------------

/**
 * Maps a `connection_state` value to a display label. Used by the Status
 * filter dropdown so operators see "Connected" not "connected".
 *
 * Verified values from connection-state.ts:
 *   connected | degraded | disconnected | pending_enrollment | revoked | archived
 */
const CONNECTION_STATE_LABELS: Record<string, string> = {
  connected: "Connected",
  degraded: "Degraded",
  disconnected: "Disconnected",
  pending_enrollment: "Pending",
  revoked: "Revoked",
  archived: "Archived",
};

function stateLabel(rawState: string): string {
  return CONNECTION_STATE_LABELS[rawState] ?? rawState;
}

// ---------------------------------------------------------------------------
// Page component
// ---------------------------------------------------------------------------

function SitesPage() {
  const { data: me } = useMe();
  const operate = canOperate(me);
  const autoLogin = canAutoLogin(me);
  // GH #255 Phase 2: agent self-update is infrastructure, not content, so gate
  // it to owner/admin (canManage), not the looser operator-capable canOperate.
  const manageAgents = canManage(me);

  // ── URL search params (P1: all filter axes) ──────────────────────────────
  // Use navigate instead of useState so filters survive reload and are shareable.
  const navigate = useNavigate({ from: Route.fullPath });
  const search = Route.useSearch();

  // The search input is URL-controlled (search.q). We write synchronously on
  // every keystroke so the URL stays in sync — TanStack Router's `replace: true`
  // pushes no history entries so the back button is not spammed.
  // Note: no debounce here because the React Compiler's rules disallow both
  // reading refs during render (the guard pattern) and synchronous setState in
  // effects (the useEffect sync pattern). The URL-as-single-source-of-truth
  // approach eliminates both concerns.
  const handleSearchChange = useCallback(
    (next: string) => {
      void navigate({
        search: (prev: SitesSearch) => ({ ...prev, q: next || undefined }),
        replace: true,
      });
    },
    [navigate],
  );

  // Memoize the fallback arrays so their stable references don't force useMemo
  // hooks downstream to re-compute on every render (the ?. [] fallback creates a
  // new array reference each time when status/tags are absent from the URL).
  const selectedStatuses = useMemo(() => search.status ?? [], [search.status]);
  const selectedTags = useMemo(() => search.tags ?? [], [search.tags]);
  const selectedAgentStatuses = useMemo(
    () => search.agentStatus ?? [],
    [search.agentStatus],
  );
  const tagMode: TagMatchMode = search.tagMode ?? "any";
  const hasTagsFilter = selectedTags.length > 0;
  const appliedClientId = search.client ?? null;
  const showArchived = search.archived ?? false;

  // ── View mode (P2) ─────────────────────────────────────────────────────────
  const [view, setView] = useSitesView(search.view, (next) => {
    void navigate({
      search: (prev: SitesSearch) => ({ ...prev, view: next }),
      replace: true,
    });
  });
  const [cardSize, setCardSize] = useCardSize();

  // ── Data ───────────────────────────────────────────────────────────────────

  const { data: clientsData } = useClients();
  const clientOptions = useMemo(
    () => (clientsData ?? []).map((c) => ({ id: c.id, name: c.name })),
    [clientsData],
  );

  const [setClientOpen, setSetClientOpen] = useState(false);

  // GH #230 "rich tags" — tag filtering is now SERVER-SIDE (?tags=&tags_match=).
  const { data: sites, isPending, isError, error, refetch, isFetching } =
    useSites({
      view: showArchived ? "archived" : "active",
      clientId: appliedClientId ?? undefined,
      tags: selectedTags,
      tagsMatch: tagMode,
    });

  const { data: tagsRegistry } = useTags();

  // Agent-release freshness rollup (agent-releases visibility). Best-effort:
  // a site-scoped collaborator (no org scope) gets a 403 here, which we treat
  // the same as "still loading": the Agent column and filter both fall back
  // to plain version text / no filter rather than surfacing a page-level error
  // for a page whose primary content is the sites list.
  const { data: fleetAgents } = useFleetAgentVersions();
  const agentStatusById = useMemo(() => {
    if (!fleetAgents) return undefined;
    return new Map<string, AgentStatus>(
      fleetAgents.sites.map((s) => [s.site_id, s.status]),
    );
  }, [fleetAgents]);
  // GH #255 Phase 2: the fleet-wide kill switch, read from the same
  // already-loaded rollup rather than a separate request. Absent (older
  // control plane, or the field not yet wired) reads as off, matching the
  // channel's "ships dark by default" contract.
  const agentSelfUpdateEnabled = fleetAgents?.self_update_enabled === true;

  // Active on any filter axis, including the (now server-side) tags filter.
  // A tag filter that matches nothing legitimately returns `sites: []`, // that must render as a filtered-empty state, never the onboarding empty
  // state (which means "this tenant has no sites at all").
  const hasActiveFilters =
    Boolean(search.q?.trim()) ||
    selectedStatuses.length > 0 ||
    hasTagsFilter ||
    Boolean(appliedClientId) ||
    selectedAgentStatuses.length > 0;

  // When the active bucket is empty (and no filters are narrowing it), also
  // fetch archived so we can surface a "Disconnected sites" panel above the
  // onboarding empty state.
  const activeIsEmpty =
    !isPending && !isError && !hasActiveFilters && (sites?.length ?? 0) === 0;
  const { data: archivedSites } = useSites({
    view: "archived",
    clientId: appliedClientId ?? undefined,
  });
  const disconnectedSites =
    activeIsEmpty && !showArchived && (archivedSites?.length ?? 0) > 0
      ? (archivedSites ?? [])
      : null;

  // Phase 5 — keep the list + detail caches live over SSE.
  useSitesLiveSync();

  // Selection and density lifted to the route so the toolbar and table share
  // the same instances.
  const selection = useSitesSelection();
  const densityState = useSitesDensity();

  const [wizardTarget, setWizardTarget] = useState<WizardTarget | null>(null);
  const [openAdminSites, setOpenAdminSites] = useState<Site[] | null>(null);
  const [agentUpdateOpen, setAgentUpdateOpen] = useState(false);

  // ── Derived filter options ─────────────────────────────────────────────────

  /**
   * Status options are derived from the actual connection_state values present
   * in the loaded data, displayed as human-readable labels. Computing from live
   * data means we never show "Revoked" if no revoked sites exist.
   *
   * The display label is what we store in selectedStatuses (the URL param) so
   * the filter works even if an operator edits the URL by hand.
   */
  const statusOptions = useMemo(() => {
    const set = new Set<string>();
    for (const s of sites ?? []) {
      const label = stateLabel(connectionStateOf(s));
      set.add(label);
    }
    return Array.from(set).sort();
  }, [sites]);

  /**
   * Agent-status options, same live-derivation rule as `statusOptions`: only
   * show a value that actually appears in the loaded sites (so "Not
   * self-updating" appears only once a wordpress.org-build site is in
   * range). Falls back to no options at all while the rollup is
   * still loading: the dropdown then simply shows "No options yet", same
   * as any other axis before its data arrives.
   */
  const agentStatusOptions = useMemo(() => {
    if (!agentStatusById) return [];
    const set = new Set<string>();
    for (const s of sites ?? []) {
      const status = agentStatusById.get(s.id) ?? "unknown";
      set.add(AGENT_STATUS_LABEL[status]);
    }
    return Array.from(set).sort();
  }, [sites, agentStatusById]);

  // GH #255: see agentStatusFilterHint's own doc for why this exists.
  const agentStatusHint = useMemo(
    () => agentStatusFilterHint(agentStatusOptions, fleetAgents?.reference_source),
    [agentStatusOptions, fleetAgents],
  );

  // ── visibleSites — pure derive over the query cache ───────────────────────
  //
  // CRITICAL INVARIANTS (preserve and do not refactor without re-reading):
  //
  // 1. `selectedSites` reads the FULL `sites` array, NOT `visibleSites`.
  //    Filtering out a site from the view must NOT remove it from bulk targets.
  //
  // 2. Selection state lives in the useSitesSelection module-level singleton.
  //    We never reconcile it against visible rows — the Set persists across
  //    filter changes by design.
  //
  // 3. The header "select all" / grid "select all" must scope to the VISIBLE
  //    (filtered) rows only — handled via visibleIds passed to the toolbar.
  //
  // 4. useSitesLiveSync is untouched here; filters are a pure client-side
  //    derive over the TanStack Query cache, not a re-fetch trigger.
  //
  // GH #230 "rich tags": the TAGS axis moved server-side (see the `useSites`
  // call above) — `sites` already reflects the tags/tagMode filter. Text
  // search still matches tag values (a tag is part of a site's searchable
  // text), but no longer independently FILTERS by tag client-side.

  const visibleSites = useMemo(() => {
    if (!sites) return [];

    const q = (search.q ?? "").trim().toLowerCase();
    const hasQ = q.length > 0;
    const hasStatus = selectedStatuses.length > 0;
    const hasAgentStatus = selectedAgentStatuses.length > 0;

    // Fast path: no active client-side filters.
    if (!hasQ && !hasStatus && !hasAgentStatus) return sites;

    return sites.filter((s) => {
      // Text search — matches name, url, or any tag.
      if (hasQ) {
        const haystack = [s.name, s.url, ...(s.tags ?? [])]
          .join(" ")
          .toLowerCase();
        if (!haystack.includes(q)) return false;
      }

      // Status filter — OR within selected statuses.
      // We compare against the display label (same value stored in the URL).
      if (hasStatus) {
        const label = stateLabel(connectionStateOf(s));
        if (!selectedStatuses.includes(label)) return false;
      }

      // Agent-status filter: same "compare against the display label"
      // convention as the connection-status filter above. A site the
      // rollup has not (yet) classified is treated as "Unknown", matching
      // the Agent column's own fallback.
      if (hasAgentStatus) {
        const status = agentStatusById?.get(s.id) ?? "unknown";
        if (!selectedAgentStatuses.includes(AGENT_STATUS_LABEL[status]))
          return false;
      }

      return true;
    });
  }, [sites, search.q, selectedStatuses, selectedAgentStatuses, agentStatusById]);

  // INVARIANT: read from the FULL sites array, not visibleSites.
  const selectedSites: Site[] = (sites ?? []).filter((s) =>
    selection.selected.has(s.id),
  );

  // Visible ids for the toolbar's "Select all" controls.
  const visibleIds = useMemo(
    () => visibleSites.map((s) => s.id),
    [visibleSites],
  );

  // ── Active filter count (for the "Clear filters" pill) ────────────────────

  const activeFilterCount = useMemo(() => {
    let count = 0;
    if (search.q?.trim()) count++;
    if (selectedStatuses.length > 0) count++;
    if (selectedTags.length > 0) count++;
    if (appliedClientId) count++;
    if (selectedAgentStatuses.length > 0) count++;
    return count;
  }, [
    search.q,
    selectedStatuses,
    selectedTags,
    appliedClientId,
    selectedAgentStatuses,
  ]);

  const handleClearAllFilters = useCallback(() => {
    void navigate({
      search: (prev: SitesSearch) => ({
        ...prev,
        q: undefined,
        status: undefined,
        tags: undefined,
        tagMode: undefined,
        client: undefined,
        agentStatus: undefined,
      }),
      replace: true,
    });
  }, [navigate]);

  // "Clear tag filters" — narrower than "Clear filters": leaves search/status/
  // client alone, only resets the tags axis (used by the tags-specific
  // zero-result state).
  const handleClearTagFilters = useCallback(() => {
    void navigate({
      search: (prev: SitesSearch) => ({ ...prev, tags: undefined, tagMode: undefined }),
      replace: true,
    });
  }, [navigate]);

  // ── Filter description for FilterEmpty ────────────────────────────────────

  const filterDescription = useMemo(() => {
    const parts: string[] = [];
    if (search.q?.trim()) parts.push(`"${search.q.trim()}"`);
    if (selectedStatuses.length > 0)
      parts.push(`status:${selectedStatuses.join(",")}`);
    if (selectedTags.length > 0) parts.push(`tags:${selectedTags.join(",")}`);
    if (appliedClientId) {
      const client = clientOptions.find((c) => c.id === appliedClientId);
      if (client) parts.push(`client:${client.name}`);
    }
    if (selectedAgentStatuses.length > 0)
      parts.push(`agent:${selectedAgentStatuses.join(",")}`);
    return parts.join(" ");
  }, [
    search.q,
    selectedStatuses,
    selectedTags,
    appliedClientId,
    clientOptions,
    selectedAgentStatuses,
  ]);

  // GH #230: sites can legitimately be `[]` because the (server-side) tags
  // filter matched nothing, not just because client-side text/status
  // filters narrowed a non-empty list — gate on `hasActiveFilters` rather
  // than `sites.length > 0`.
  const showFilterEmpty =
    !isPending && !isError && hasActiveFilters && visibleSites.length === 0;

  // ── Auto-login ─────────────────────────────────────────────────────────────

  const bulkBackup = useBulkBackup();

  const loginMutation = useAutoLogin();
  const openAutoLoginRef = useRef((_site: Site) => {});

  const handleOpenAutoLogin = useCallback(
    (site: Site) => {
      if (!autoLogin) return;
      toast.info(`Opening ${site.name}`);
      loginMutation.mutate(
        { siteId: site.id },
        {
          onSuccess: (data) => {
            window.open(data.redirect_url, "_blank", "noopener,noreferrer");
          },
          onError: (err) => {
            toast.error(`Could not open ${site.name}`, {
              description: err.message,
              action: {
                label: "Try again",
                onClick: () => openAutoLoginRef.current(site),
              },
            });
          },
        },
      );
    },
    [autoLogin, loginMutation],
  );

  useEffect(() => {
    openAutoLoginRef.current = handleOpenAutoLogin;
  }, [handleOpenAutoLogin]);

  // "Open site" menu action — navigates to the site's detail page. Wired here
  // (rather than left as a prop the caller forgets to pass) so it works
  // identically in both the grid (card) and table views (GH #150).
  const handleOpenDetail = useCallback(
    (site: Site) => {
      void navigate({ to: "/sites/$siteId", params: { siteId: site.id } });
    },
    [navigate],
  );

  // ── Disconnect / Undo / Reconnect ─────────────────────────────────────────

  const revoke = useRevokeSite();
  const restore = useRestoreSite();
  const enrollmentCode = useCreateEnrollmentCode();

  const deleteSite = useDeleteSite();
  const [bulkDeleteOpen, setBulkDeleteOpen] = useState(false);
  const [bulkDeleting, setBulkDeleting] = useState(false);

  const [disconnectTarget, setDisconnectTarget] = useState<Site | null>(null);
  const [removeTarget, setRemoveTarget] = useState<Site | null>(null);
  const [removing, setRemoving] = useState(false);

  const [reconnectTarget, setReconnectTarget] = useState<{
    siteId: string;
    url: string;
    enrollmentCode: string;
    expiresAt: string;
  } | null>(null);

  const [onboardingUrl, setOnboardingUrl] = useState<string | null>(null);

  const handleDisconnect = useCallback((site: Site) => {
    setDisconnectTarget(site);
  }, []);

  const confirmDisconnect = useCallback(() => {
    const site = disconnectTarget;
    if (!site) return;
    revoke.mutate(
      { siteId: site.id, reason: "operator disconnect" },
      {
        onSuccess: () => {
          setDisconnectTarget(null);
          const host = hostOf(site.url);
          toast.success(`${host} disconnected.`, {
            description: "Backups and monitoring are paused. History is kept.",
            action: {
              label: "Undo",
              onClick: () => {
                restore.mutate(
                  { siteId: site.id },
                  {
                    onSuccess: () =>
                      toast.success(`${host} reconnecting...`),
                    onError: (err) =>
                      toast.error(`Could not undo`, {
                        description: err.message,
                      }),
                  },
                );
              },
            },
          });
        },
        onError: (err) => {
          toast.error(`Could not disconnect ${hostOf(site.url)}`, {
            description: err.message,
          });
        },
      },
    );
  }, [disconnectTarget, revoke, restore]);

  const handleReconnect = useCallback(
    (site: Site) => {
      toast.info(`Generating an enrollment code for ${hostOf(site.url)}...`);
      enrollmentCode.mutate(
        { siteId: site.id },
        {
          onSuccess: (result) => {
            setReconnectTarget({
              siteId: site.id,
              url: site.url,
              enrollmentCode: result.enrollment_code,
              expiresAt: result.expires_at,
            });
          },
          onError: (err) => {
            toast.error(`Could not start reconnecting ${hostOf(site.url)}`, {
              description: err.message,
            });
          },
        },
      );
    },
    [enrollmentCode],
  );

  const handleRemove = useCallback((site: Site) => {
    setRemoveTarget(site);
  }, []);

  const confirmRemove = useCallback(async () => {
    const site = removeTarget;
    if (!site) return;
    setRemoving(true);
    try {
      await deleteSite.mutateAsync(site.id);
      setRemoveTarget(null);
      toast.success(`${hostOf(site.url)} removed.`, {
        description: "The WPMgr record has been deleted. The WordPress site itself is untouched.",
      });
    } catch (err) {
      toast.error(`Could not remove ${hostOf(site.url)}`, {
        description: err instanceof Error ? err.message : "An unexpected error occurred.",
      });
    } finally {
      setRemoving(false);
    }
  }, [removeTarget, deleteSite]);

  // ── Bulk action handlers ───────────────────────────────────────────────────

  const openUpdateWizardForSelection = useCallback(
    (kind: "plugins" | "themes" | "core") => {
      setWizardTarget({
        kind: "sites",
        siteIds: Array.from(selection.selected),
        // Pass the chosen target kind so the wizard can pre-scope to plugins,
        // themes, or core and default to showing only items with updates.
        updateKind: kind,
      });
    },
    [selection],
  );

  // GH #255 Phase 2: opens the agent self-update confirmation dialog for the
  // current selection. The toolbar item itself is hidden unless manageAgents
  // && agentSelfUpdateEnabled (see onUpdateAgent below), so reaching this
  // handler already implies both.
  const handleUpdateAgent = useCallback(() => {
    if (selection.count === 0) return;
    setAgentUpdateOpen(true);
  }, [selection.count]);

  const handleBulkBackup = useCallback(async () => {
    const ids = Array.from(selection.selected);
    if (ids.length === 0) return;
    const { enqueued, skipped, failed } = await bulkBackup(ids);

    // Toast AFTER the calls settle with honest counts.
    const total = ids.length;
    const isSingle = total === 1;
    const firstId = ids[0];

    // Resolve the "View activity" navigation target: single site → site activity
    // page; multi-site → fleet backups view. Both routes exist.
    const activityHref =
      isSingle && firstId
        ? `/sites/${firstId}/activity`
        : "/backups";

    if (failed.length > 0 && enqueued.length === 0 && skipped.length === 0) {
      toast.error(
        `Backup failed for ${failed.length} ${failed.length === 1 ? "site" : "sites"}`,
        { description: "Check the site connection and try again." },
      );
    } else {
      const parts: string[] = [];
      if (enqueued.length > 0)
        parts.push(`${enqueued.length} queued`);
      if (skipped.length > 0)
        parts.push(`${skipped.length} already running`);
      if (failed.length > 0)
        parts.push(`${failed.length} failed`);

      toast.success(`Backup started for ${enqueued.length + skipped.length} of ${total} ${total === 1 ? "site" : "sites"}`, {
        description: parts.join(", "),
        action: {
          label: "View activity",
          onClick: () => {
            void navigate({
              // eslint-disable-next-line @typescript-eslint/no-explicit-any
              to: activityHref as any,
            });
          },
        },
      });
    }
  }, [selection, bulkBackup, navigate]);

  const handleBulkRestore = useCallback(() => {
    toast.info("Fleet-wide restore lands in Sprint 4");
  }, []);

  const handleBulkOpenWpAdmin = useCallback(() => {
    if (!autoLogin) {
      toast.error("Auto-login requires admin permissions", {
        description: "Ask an admin to grant the role, then retry.",
      });
      return;
    }
    const ids = Array.from(selection.selected);
    const targets = ids
      .map((id) => selectedSites.find((s) => s.id === id))
      .filter((s): s is Site => s !== undefined);

    if (targets.length === 0) return;

    // Show the persistent panel listing ALL selected sites. The panel resolves
    // auto-login URLs itself and lets the operator open each site individually
    // (each click = its own user gesture, so popups are allowed) or all at once.
    // This replaces the toast fan-out that was capped at ~5 visible toasts and
    // lost sites when the first tab stole focus.
    setOpenAdminSites(targets);
  }, [autoLogin, selectedSites, selection]);

  // GH #230 "rich tags" — opens the bulk tag-edit drawer (tri-state TagPicker
  // + "Apply to N sites"). Targets come from the FULL `selection.selected`
  // set, matching every other bulk action's invariant (never `visibleSites`).
  const bulkAction = useBulkAction();
  const handleBulkTag = useCallback(() => {
    const ids = Array.from(selection.selected);
    if (ids.length === 0) return;
    bulkAction.openTagEdit(ids);
  }, [selection, bulkAction]);

  const handleBulkSetClient = useCallback(() => {
    setSetClientOpen(true);
  }, []);

  const handleBulkPauseMonitoring = useCallback(() => {
    toast.info(`Pausing monitoring on ${selection.count} sites lands in Sprint 4`);
  }, [selection.count]);

  const handleBulkDelete = useCallback(() => {
    if (selection.count === 0) return;
    setBulkDeleteOpen(true);
  }, [selection.count]);

  const confirmBulkDelete = useCallback(async () => {
    const ids = Array.from(selection.selected);
    if (ids.length === 0) {
      setBulkDeleteOpen(false);
      return;
    }
    setBulkDeleting(true);
    const results = await Promise.allSettled(
      ids.map((id) => deleteSite.mutateAsync(id)),
    );
    const failed = results.filter((r) => r.status === "rejected").length;
    const ok = ids.length - failed;
    setBulkDeleting(false);
    setBulkDeleteOpen(false);
    selection.replace([]);
    if (failed > 0) {
      toast.error(`Deleted ${ok} of ${ids.length} sites; ${failed} failed`);
    } else {
      toast.success(`Deleted ${ok} ${ok === 1 ? "site" : "sites"}`);
    }
  }, [selection, deleteSite]);

  // ── Render ─────────────────────────────────────────────────────────────────

  return (
    <section aria-labelledby="sites-heading" className="space-y-4">
      <PageHeader
        title="Sites"
        subline={
          isPending
            ? undefined
            : sites !== undefined && sites.length > 0
              ? `${sites.length} site${sites.length === 1 ? "" : "s"} enrolled`
              : undefined
        }
        actions={operate ? <AddSiteDialog /> : undefined}
      />

      {isPending ? (
        // Show the appropriate skeleton based on the current view.
        view === "grid" ? (
          <SitesGridSkeleton />
        ) : (
          <SitesTableSkeleton />
        )
      ) : isError ? (
        <PageError
          what="Could not load sites."
          why={error instanceof Error ? error.message : "The server returned an unexpected response."}
          onRetry={() => void refetch()}
          retryLabel="Reload sites"
          isRetrying={isFetching}
        />
      ) : sites.length === 0 && !hasActiveFilters ? (
        <>
          {disconnectedSites ? (
            <DisconnectedSitesPanel
              sites={disconnectedSites}
              onOpenDetail={handleOpenDetail}
              onReconnect={operate ? handleReconnect : undefined}
              onRemove={operate ? handleRemove : undefined}
            />
          ) : null}
          <SitesPageEmpty
            // GH #252: NoSitesEmpty (rendered here once onboarding has been
            // dismissed on this browser) defaults its own `cta` to another
            // <AddSiteDialog />. The PageHeader's action above already owns
            // the page's one Add-site trigger, so suppress this one for
            // BOTH roles (not just non-operators) rather than let a second
            // "Add site" button appear alongside the header's.
            cta={<AddSitePlaceholder />}
            onOnboardingHandoff={operate ? ({ url }) => setOnboardingUrl(url) : undefined}
          />
        </>
      ) : (
        <>
          <SitesToolbar
            selection={selection}
            densityState={densityState}
            search={search.q ?? ""}
            onSearchChange={handleSearchChange}
            tagRegistry={tagsRegistry ?? []}
            selectedTags={selectedTags}
            onTagToggle={(tag) => {
              const next = selectedTags.includes(tag)
                ? selectedTags.filter((t) => t !== tag)
                : [...selectedTags, tag];
              void navigate({
                search: (prev: SitesSearch) => ({
                  ...prev,
                  tags: next.length ? next : undefined,
                  // A match-mode choice is only meaningful with 2+ tags;
                  // drop it once the selection falls below that.
                  tagMode: next.length >= 2 ? prev.tagMode : undefined,
                }),
                replace: true,
              });
            }}
            onTagsClear={() => {
              void navigate({
                search: (prev: SitesSearch) => ({ ...prev, tags: undefined, tagMode: undefined }),
                replace: true,
              });
            }}
            tagMode={tagMode}
            onTagModeChange={(mode) => {
              void navigate({
                search: (prev: SitesSearch) => ({ ...prev, tagMode: mode === "any" ? undefined : mode }),
                replace: true,
              });
            }}
            statusOptions={statusOptions}
            selectedStatuses={selectedStatuses}
            onStatusToggle={(status) => {
              const next = selectedStatuses.includes(status)
                ? selectedStatuses.filter((s) => s !== status)
                : [...selectedStatuses, status];
              void navigate({
                search: (prev: SitesSearch) => ({ ...prev, status: next.length ? next : undefined }),
                replace: true,
              });
            }}
            onStatusesClear={() => {
              void navigate({
                search: (prev: SitesSearch) => ({ ...prev, status: undefined }),
                replace: true,
              });
            }}
            agentStatusOptions={agentStatusOptions}
            selectedAgentStatuses={selectedAgentStatuses}
            agentStatusHint={agentStatusHint}
            onAgentStatusToggle={(status) => {
              const next = selectedAgentStatuses.includes(status)
                ? selectedAgentStatuses.filter((s) => s !== status)
                : [...selectedAgentStatuses, status];
              void navigate({
                search: (prev: SitesSearch) => ({
                  ...prev,
                  agentStatus: next.length ? next : undefined,
                }),
                replace: true,
              });
            }}
            onAgentStatusesClear={() => {
              void navigate({
                search: (prev: SitesSearch) => ({ ...prev, agentStatus: undefined }),
                replace: true,
              });
            }}
            clientOptions={clientOptions}
            appliedClientId={appliedClientId}
            onClientFilterChange={(clientId) => {
              void navigate({
                search: (prev: SitesSearch) => ({ ...prev, client: clientId ?? undefined }),
                replace: true,
              });
            }}
            activeFilterCount={activeFilterCount}
            onClearAllFilters={handleClearAllFilters}
            view={view}
            onViewChange={setView}
            cardSize={cardSize}
            onCardSizeChange={setCardSize}
            visibleIds={visibleIds}
            canOperate={operate}
            onBulkUpdate={openUpdateWizardForSelection}
            onBulkBackup={() => { void handleBulkBackup(); }}
            onBulkRestore={handleBulkRestore}
            onBulkOpenWpAdmin={handleBulkOpenWpAdmin}
            onBulkTag={handleBulkTag}
            onBulkSetClient={handleBulkSetClient}
            onBulkPauseMonitoring={handleBulkPauseMonitoring}
            onBulkDelete={handleBulkDelete}
            // GH #255 Phase 2: undefined hides the menu item entirely; the
            // action must not appear usable while the control-plane kill
            // switch is off, and it is infrastructure (owner/admin only).
            onUpdateAgent={
              manageAgents && agentSelfUpdateEnabled
                ? handleUpdateAgent
                : undefined
            }
            // GH #252: the primary "Add site" action lives in the PageHeader
            // only (top-right, matching every other list page's primary
            // action), so don't duplicate the trigger here. Non-operators
            // keep the placeholder slot wired so the affordance point stays
            // in exactly one place.
            addSiteSlot={operate ? undefined : <AddSitePlaceholder />}
          />

          {/* Phase 5 — archived filter chip */}
          {operate ? (
            <div className="flex items-center gap-2">
              <button
                type="button"
                aria-pressed={showArchived}
                onClick={() => {
                  void navigate({
                    search: (prev: SitesSearch) => ({
                      ...prev,
                      archived: showArchived ? undefined : true,
                    }),
                    replace: true,
                  });
                }}
                className={cn(
                  "inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
                  showArchived
                    ? "border-primary bg-primary/10 text-foreground"
                    : "border-border text-muted-foreground hover:text-foreground",
                )}
              >
                <Archive aria-hidden="true" className="size-3.5" />
                {showArchived ? "Showing archived" : "Show archived"}
              </button>
            </div>
          ) : null}

          {showFilterEmpty ? (
            // The tag-specific empty state applies only when the (server-
            // side) tags filter is the reason `sites` itself came back
            // empty. If `sites` has rows but a client-side axis (search or
            // status) narrowed them to zero, the generic FilterEmpty is the
            // accurate message even when tags are also selected.
            hasTagsFilter && (sites?.length ?? 0) === 0 ? (
              <TagsFilterEmpty tags={selectedTags} onClear={handleClearTagFilters} />
            ) : (
              <FilterEmpty
                description={filterDescription}
                onClearFilters={handleClearAllFilters}
              />
            )
          ) : view === "grid" ? (
            <SitesGrid
              sites={visibleSites}
              cardSize={cardSize}
              onOpenAutoLogin={autoLogin ? handleOpenAutoLogin : undefined}
              onOpenDetail={handleOpenDetail}
              onDisconnect={operate ? handleDisconnect : undefined}
              onReconnect={operate ? handleReconnect : undefined}
            />
          ) : (
            <SitesTable
              sites={visibleSites}
              isLoading={isPending}
              selection={operate ? selection : undefined}
              densityState={densityState}
              agentStatusById={agentStatusById}
              agentReferenceSource={fleetAgents?.reference_source}
              agentReferenceCheck={fleetAgents?.agent_mirror}
              onOpenAutoLogin={autoLogin ? handleOpenAutoLogin : undefined}
              onOpenDetail={handleOpenDetail}
              onDisconnect={operate ? handleDisconnect : undefined}
              onReconnect={operate ? handleReconnect : undefined}
            />
          )}
        </>
      )}

      {operate ? (
        <UpdateWizard
          open={wizardTarget !== null}
          onClose={() => setWizardTarget(null)}
          target={wizardTarget}
          sites={
            wizardTarget?.kind === "sites" && selectedSites.length > 0
              ? selectedSites
              : (sites ?? [])
          }
        />
      ) : null}

      {manageAgents ? (
        <AgentSelfUpdateDialog
          open={agentUpdateOpen}
          onClose={() => setAgentUpdateOpen(false)}
          sites={selectedSites}
          agentStatusById={agentStatusById}
          agentReferenceSource={fleetAgents?.reference_source}
        />
      ) : null}

      {operate ? (
        <DestructiveConfirm
          open={disconnectTarget !== null}
          onClose={() => setDisconnectTarget(null)}
          onConfirm={confirmDisconnect}
          title={`Disconnect ${disconnectTarget ? hostOf(disconnectTarget.url) : "site"}`}
          resourceName={disconnectTarget ? hostOf(disconnectTarget.url) : ""}
          confirmLabel="Disconnect site"
          cancelLabel="Keep connected"
          isPending={revoke.isPending}
          errorMessage={revoke.isError ? revoke.error.message : null}
          consequencesBody={
            <div className="space-y-2">
              <p>
                We'll send a revoke to the agent on its next heartbeat
                (within ~60 seconds). The agent stops accepting commands and
                clears its credentials.
              </p>
              <p>
                Backups and monitoring stop. The site is archived with its full
                history kept. You can reconnect later.
              </p>
            </div>
          }
        />
      ) : null}

      {operate ? (
        <DestructiveConfirm
          open={bulkDeleteOpen}
          onClose={() => setBulkDeleteOpen(false)}
          onConfirm={confirmBulkDelete}
          title={`Delete ${selection.count} ${selection.count === 1 ? "site" : "sites"}`}
          resourceName={String(selection.count)}
          confirmLabel={`Delete ${selection.count === 1 ? "site" : "sites"}`}
          cancelLabel="Keep sites"
          isPending={bulkDeleting}
          consequencesBody={
            <div className="space-y-2">
              <p>
                This permanently removes{" "}
                {selection.count === 1
                  ? "this site"
                  : `these ${selection.count} sites`}{" "}
                and all associated WPMgr history (backup metadata, scans,
                monitoring, activity). The WordPress site itself is not touched,
                only its WPMgr record.
              </p>
              <p>
                To stop managing a site without losing its history, disconnect
                or archive it instead.
              </p>
              <p>
                Type <strong>{selection.count}</strong> to confirm.
              </p>
            </div>
          }
        />
      ) : null}

      {operate ? (
        <DestructiveConfirm
          open={removeTarget !== null}
          onClose={() => setRemoveTarget(null)}
          onConfirm={confirmRemove}
          title={`Remove ${removeTarget ? hostOf(removeTarget.url) : "site"}`}
          resourceName={removeTarget ? hostOf(removeTarget.url) : ""}
          confirmLabel="Remove site"
          cancelLabel="Keep site"
          isPending={removing}
          errorMessage={deleteSite.isError ? deleteSite.error.message : null}
          consequencesBody={
            <div className="space-y-2">
              <p>
                This permanently removes the WPMgr record for{" "}
                <strong>{removeTarget ? hostOf(removeTarget.url) : "this site"}</strong>{" "}
                and all associated history (backup metadata, scans, monitoring,
                activity).
              </p>
              <p>
                The WordPress site itself is not touched. Nothing is changed or
                deleted on the server.
              </p>
              <p>
                Type{" "}
                <strong>{removeTarget ? hostOf(removeTarget.url) : ""}</strong>{" "}
                to confirm.
              </p>
            </div>
          }
        />
      ) : null}

      {operate ? (
        <AddSiteDialog
          open={onboardingUrl !== null}
          onClose={() => setOnboardingUrl(null)}
          initialUrl={onboardingUrl ?? undefined}
        />
      ) : null}

      {operate ? (
        <AddSiteDialog
          open={reconnectTarget !== null}
          onClose={() => setReconnectTarget(null)}
          initialSite={reconnectTarget ?? undefined}
        />
      ) : null}

      {operate ? (
        <SetClientDialog
          open={setClientOpen}
          onClose={() => setSetClientOpen(false)}
          siteIds={Array.from(selection.selected)}
          onSuccess={() => selection.replace([])}
        />
      ) : null}

      {/* Persistent "Open in wp-admin" panel — replaces the toast fan-out so
          all N sites are reachable regardless of browser tab-focus or Sonner's
          visible-toast cap. Rendered outside the auth gate because it handles
          its own permission check (canAutoLogin) before opening. */}
      <OpenInAdminPanel
        sites={openAdminSites}
        onClose={() => setOpenAdminSites(null)}
      />
    </section>
  );
}

// ---------------------------------------------------------------------------
// Loading skeletons
// ---------------------------------------------------------------------------

function SitesTableSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading sites"
      className="overflow-hidden rounded-lg border border-border"
    >
      <span className="sr-only">Loading sites</span>
      <div className="flex h-11 items-center gap-4 border-b border-border bg-background px-4">
        <Skeleton className="size-4 rounded" />
        <Skeleton className="h-3 w-24" />
        <Skeleton className="ml-4 h-3 w-16" />
        <Skeleton className="ml-auto h-3 w-12" />
      </div>
      {Array.from({ length: 6 }).map((_, i) => (
        <div
          key={i}
          className="flex h-14 items-center gap-4 border-b border-border px-4 last:border-0"
        >
          <Skeleton className="size-4 rounded" />
          <div className="flex flex-1 flex-col gap-1.5">
            <Skeleton className="h-3.5 w-48" />
            <Skeleton className="h-2.5 w-24" />
          </div>
          <Skeleton className="h-5 w-16 rounded-sm" />
          <Skeleton className="h-3.5 w-12 font-mono" />
          <Skeleton className="h-3.5 w-10 font-mono" />
          <Skeleton className="h-6 w-6 rounded" />
          <Skeleton className="h-6 w-6 rounded" />
        </div>
      ))}
    </div>
  );
}

// Read-only operators see the toolbar without an Add Site primary.
function AddSitePlaceholder() {
  return null;
}

/**
 * Compact panel shown above the onboarding empty state when the active bucket
 * is empty but there are archived/disconnected sites.
 */
function DisconnectedSitesPanel({
  sites,
  onOpenDetail,
  onReconnect,
  onRemove,
}: {
  sites: Site[];
  onOpenDetail?: (site: Site) => void;
  onReconnect?: (site: Site) => void;
  onRemove?: (site: Site) => void;
}) {
  const count = sites.length;
  return (
    <section aria-labelledby="disconnected-sites-heading" className="space-y-3">
      <p
        id="disconnected-sites-heading"
        className="text-sm font-medium text-muted-foreground"
      >
        {count === 1
          ? "You have 1 disconnected site"
          : `You have ${count} disconnected sites`}
      </p>
      <SitesTable
        sites={sites}
        isLoading={false}
        onOpenDetail={onOpenDetail}
        onReconnect={onReconnect}
        onRemove={onRemove}
      />
    </section>
  );
}

/**
 * GH #230 "rich tags" — the tag-specific zero-result state, shown when the
 * server-side tags filter matched no sites. Distinct from the generic
 * `FilterEmpty` because the fix is narrower (drop just the tags axis, not
 * every filter) and the operator benefits from seeing exactly which tags
 * came up empty.
 */
function TagsFilterEmpty({
  tags,
  onClear,
}: {
  tags: readonly string[];
  onClear: () => void;
}) {
  const colorMap = useTagColorMap();
  return (
    <div
      role="status"
      aria-label="No sites match these tags"
      className="flex flex-col items-center gap-3 py-12 text-center"
    >
      <Search
        aria-hidden="true"
        strokeWidth={1.5}
        className="size-8 text-muted-foreground/50"
      />
      <p className="text-balance text-sm text-foreground">No sites match these tags</p>
      <div className="flex flex-wrap items-center justify-center gap-1">
        {tags.map((name) => (
          <TagChip key={name} tag={{ name, color: colorMap.get(name) }} />
        ))}
      </div>
      <button
        type="button"
        onClick={onClear}
        className="inline-flex items-center gap-1 text-sm font-medium text-primary underline-offset-4 transition-colors hover:underline focus-visible:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
      >
        Clear tag filters
      </button>
    </div>
  );
}

function hostOf(url: string): string {
  try {
    return new URL(url).hostname || url;
  } catch {
    return url.replace(/^https?:\/\//i, "").replace(/\/$/, "");
  }
}
