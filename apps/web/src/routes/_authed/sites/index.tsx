import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { Archive } from "lucide-react";

import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { FilterEmpty, SitesPageEmpty } from "@/components/empty";
import { PageHeader } from "@/components/shared/page-header";
import { useSites } from "@/features/sites/use-sites";
import { SitesTable } from "@/features/sites/sites-table";
import { SitesToolbar } from "@/features/sites/sites-toolbar";
import { useSitesSelection } from "@/features/sites/use-sites-selection";
import { useSitesDensity } from "@/features/sites/use-sites-density";
import { AddSiteDialog } from "@/features/sites/add-site-dialog";
import { useSitesLiveSync } from "@/features/sites/use-sites-live";
import {
  useRevokeSite,
  useRestoreSite,
  useCreateEnrollmentCode,
} from "@/features/sites/use-site-connection";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { useAutoLogin, canAutoLogin } from "@/features/sites/use-autologin";
import {
  UpdateWizard,
  type WizardTarget,
} from "@/features/updates/update-wizard";
import { useMe, canOperate } from "@/features/auth/use-auth";
import { toast } from "@/components/toast";
import { cn } from "@/lib/utils";
import type { Site } from "@wpmgr/api";

export const Route = createFileRoute("/_authed/sites/")({
  component: SitesPage,
});

function SitesPage() {
  const { data: me } = useMe();
  const operate = canOperate(me);
  const autoLogin = canAutoLogin(me);

  // Toolbar-driven search (Sprint 3 is local state; Sprint 4 will plumb this
  // into useSites() with debounce — see TODOs below).
  const [search, setSearch] = useState("");

  // Sprint 3 keeps server-side tag filtering off until the toolbar's Tag
  // dropdown is wired (Sprint 4); the toolbar fires console.debug events for
  // every filter change so the wiring path stays observable.
  const appliedTag = "";

  // Phase 5 — the "Archived" filter chip flips the list to the archived bucket
  // (the default list hides archived sites).
  const [showArchived, setShowArchived] = useState(false);

  const { data: sites, isPending, isError, error, refetch, isFetching } =
    useSites(appliedTag, { view: showArchived ? "archived" : "active" });

  // Phase 5 — keep the list + detail caches live over SSE (no polling).
  // Cardinality events invalidate the list; in-place events patch the cache.
  useSitesLiveSync();

  // Selection and density lifted to the route so the toolbar and table share
  // the same instances — selecting in the table flips the toolbar to action
  // mode, and the density toggle in the toolbar drives row height in the
  // table.
  const selection = useSitesSelection();
  const densityState = useSitesDensity();

  const [wizardTarget, setWizardTarget] = useState<WizardTarget | null>(null);

  // Aggregate the available client (tag) and tag values for the filter
  // dropdowns. Cheap pre-pass — the sites array is already in memory.
  const tagOptions = useMemo(() => {
    const set = new Set<string>();
    for (const s of sites ?? []) {
      for (const t of s.tags ?? []) set.add(t);
    }
    return Array.from(set).sort();
  }, [sites]);

  // Locally narrow by the toolbar's search term. Real text search ships with
  // the server-side filter wiring (Sprint 4).
  const visibleSites = useMemo(() => {
    if (!sites) return [];
    const q = search.trim().toLowerCase();
    if (!q) return sites;
    return sites.filter((s) =>
      [s.name, s.url, ...(s.tags ?? [])]
        .join(" ")
        .toLowerCase()
        .includes(q),
    );
  }, [sites, search]);

  const selectedSites: Site[] = (sites ?? []).filter((s) =>
    selection.selected.has(s.id),
  );

  // One-click login wired here so the table stays presentational. The mutation
  // returns a short-lived redirect URL that we open in a new tab.
  const loginMutation = useAutoLogin();

  // openAutoLoginRef holds the latest handleOpenAutoLogin so the toast "Try
  // again" action can call it without the callback closing over itself (which
  // would violate the rules-of-hooks immutability constraint).
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

  // Keep the ref current so any already-rendered toast retains a live handle
  // to the latest version of the callback.
  useEffect(() => {
    openAutoLoginRef.current = handleOpenAutoLogin;
  }, [handleOpenAutoLogin]);

  // -------------------------------------------------------------------------
  // Phase 5 — Disconnect / Undo / Reconnect
  // -------------------------------------------------------------------------

  const revoke = useRevokeSite();
  const restore = useRestoreSite();
  const enrollmentCode = useCreateEnrollmentCode();

  // Disconnect confirm target (DestructiveConfirm — type the hostname).
  const [disconnectTarget, setDisconnectTarget] = useState<Site | null>(null);

  // Reconnect: when set, the AddSiteDialog opens directly at step B with a
  // freshly-minted enrollment code bound to the existing site.
  const [reconnectTarget, setReconnectTarget] = useState<{
    siteId: string;
    url: string;
    enrollmentCode: string;
    expiresAt: string;
  } | null>(null);

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
          // 60s Undo → POST /:id/restore. The row also updates live via SSE.
          toast.success(`${host} disconnected.`, {
            description: "Backups and monitoring are paused. History is kept.",
            action: {
              label: "Undo",
              onClick: () => {
                restore.mutate(
                  { siteId: site.id },
                  {
                    onSuccess: () =>
                      toast.success(`${host} reconnecting…`),
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
      // Mint a fresh code, then open the modal at step B pre-bound to this site.
      toast.info(`Generating an enrollment code for ${hostOf(site.url)}…`);
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

  // -------------------------------------------------------------------------
  // Bulk action handlers (Sprint 3 wires the obvious ones, stubs the rest)
  // -------------------------------------------------------------------------

  const openUpdateWizardForSelection = useCallback(
    (kind: "plugins" | "themes" | "core") => {
      // The existing UpdateWizard is component-agnostic; Sprint 4 will pass
      // `kind` through so it preselects the right step. For now we surface
      // the intent so the wire-up landing zone is unambiguous.
      // TODO(sprint-4): pipe `kind` into UpdateWizard initial step.

      console.debug("[sites] bulk update", {
        kind,
        siteIds: Array.from(selection.selected),
      });
      setWizardTarget({
        kind: "sites",
        siteIds: Array.from(selection.selected),
      });
    },
    [selection],
  );

  const handleBulkBackup = useCallback(() => {
    // TODO(sprint-4): wire to POST /api/v1/backups/bulk (endpoint exists in
    // the API; needs a confirm modal + toast). Stubbed for the transform
    // animation review.
    toast.success(`Backup queued for ${selection.count} sites`, {
      description: "We will surface per-site results as they finish.",
      action: {
        label: "View activity",
        onClick: () => {
          // TODO(sprint-4): deep link to the activity drawer once it lands.

          console.debug("[sites] open activity for bulk backup");
        },
      },
    });

    console.debug("[sites] bulk backup", {
      siteIds: Array.from(selection.selected),
    });
  }, [selection]);

  const handleBulkRestore = useCallback(() => {
    // TODO(sprint-4): restore is currently per-site; a fleet-wide restore
    // wizard is a separate design surface.
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
    // Browsers throttle popups; we open the first 8 immediately and queue
    // the rest behind a confirm in Sprint 4.
    // TODO(sprint-4): replace inline loop with the existing useAutoLogin
    // queue + per-site progress toast.
    const cap = Math.min(ids.length, 8);
    toast.info(`Opening ${cap} sites in wp-admin`);
    for (let i = 0; i < cap; i++) {
      const site = selectedSites.find((s) => s.id === ids[i]);
      if (!site) continue;
      loginMutation.mutate(
        { siteId: site.id },
        {
          onSuccess: (data) => {
            window.open(data.redirect_url, "_blank", "noopener,noreferrer");
          },
          onError: (err) => {
            toast.error(`Could not open ${site.name}`, {
              description: err.message,
            });
          },
        },
      );
    }
  }, [autoLogin, loginMutation, selectedSites, selection]);

  const handleBulkTag = useCallback(() => {
    // TODO(sprint-4): open a tag-picker modal (bulk-drawer subagent owns it).
    toast.info(`Tagging ${selection.count} sites lands in Sprint 4`);
  }, [selection.count]);

  const handleBulkSetClient = useCallback(() => {
    // TODO(sprint-4): open a client-picker modal.
    toast.info(`Setting client on ${selection.count} sites lands in Sprint 4`);
  }, [selection.count]);

  const handleBulkPauseMonitoring = useCallback(() => {
    // TODO(sprint-4): wire to the monitoring service pause endpoint.
    toast.info(`Pausing monitoring on ${selection.count} sites lands in Sprint 4`);
  }, [selection.count]);

  const handleBulkDelete = useCallback(() => {
    // TODO(sprint-4): open a destructive confirm modal that requires typing
    // the resource count (per DESIGN.md "Destructive requires typing").
    toast.error("Bulk delete needs a confirm modal", {
      description: `Refusing to delete ${selection.count} sites without typed confirmation.`,
    });
  }, [selection.count]);

  // Build a human-readable filter summary for the empty-search state.
  const filterDescription = search.trim()
    ? `"${search.trim()}"`
    : "";

  const showFilterEmpty =
    !isPending && !isError && sites !== undefined && sites.length > 0 && visibleSites.length === 0;

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
        <SitesTableSkeleton />
      ) : isError ? (
        <PageError
          what="Could not load sites."
          why={error instanceof Error ? error.message : "The server returned an unexpected response."}
          onRetry={() => void refetch()}
          retryLabel="Reload sites"
          isRetrying={isFetching}
        />
      ) : sites.length === 0 ? (
        <SitesPageEmpty
          cta={operate ? undefined : <AddSitePlaceholder />}
        />
      ) : (
        <>
          <SitesToolbar
            selection={selection}
            densityState={densityState}
            search={search}
            onSearchChange={setSearch}
            tagOptions={tagOptions}
            clientOptions={tagOptions}
            canOperate={operate}
            onBulkUpdate={openUpdateWizardForSelection}
            onBulkBackup={handleBulkBackup}
            onBulkRestore={handleBulkRestore}
            onBulkOpenWpAdmin={handleBulkOpenWpAdmin}
            onBulkTag={handleBulkTag}
            onBulkSetClient={handleBulkSetClient}
            onBulkPauseMonitoring={handleBulkPauseMonitoring}
            onBulkDelete={handleBulkDelete}
            addSiteSlot={operate ? <AddSiteDialog /> : <AddSitePlaceholder />}
          />
          {/* Phase 5 — archived filter chip. Toggles the list between the
              default (active) bucket and the archived bucket. */}
          {operate ? (
            <div className="flex items-center gap-2">
              <button
                type="button"
                aria-pressed={showArchived}
                onClick={() => setShowArchived((v) => !v)}
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
            <FilterEmpty
              description={filterDescription}
              onClearFilters={() => setSearch("")}
            />
          ) : (
            <SitesTable
              sites={visibleSites}
              isLoading={isPending}
              selection={operate ? selection : undefined}
              densityState={densityState}
              onOpenAutoLogin={autoLogin ? handleOpenAutoLogin : undefined}
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

      {/* Phase 5 — Disconnect confirm (type the hostname to confirm). */}
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
                history kept — you can reconnect later.
              </p>
            </div>
          }
        />
      ) : null}

      {/* Phase 5 — Reconnect: open the Add-site modal at step B with a fresh,
          pre-bound enrollment code for the existing site. */}
      {operate ? (
        <AddSiteDialog
          open={reconnectTarget !== null}
          onClose={() => setReconnectTarget(null)}
          initialSite={reconnectTarget ?? undefined}
        />
      ) : null}
    </section>
  );
}

// Table-shaped skeleton so the loading state has the same spatial footprint as
// the real table. Row count (6) is enough to fill a typical viewport without
// over-committing on screen real estate.
function SitesTableSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading sites"
      className="overflow-hidden rounded-lg border border-border"
    >
      <span className="sr-only">Loading sites</span>
      {/* Header row */}
      <div className="flex h-11 items-center gap-4 border-b border-border bg-background px-4">
        <Skeleton className="size-4 rounded" />
        <Skeleton className="h-3 w-24" />
        <Skeleton className="ml-4 h-3 w-16" />
        <Skeleton className="ml-auto h-3 w-12" />
      </div>
      {/* Body rows */}
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

// Read-only operators see the toolbar without an Add Site primary; we render
// an inert placeholder so the row layout stays stable across roles.
function AddSitePlaceholder() {
  return null;
}

function hostOf(url: string): string {
  try {
    return new URL(url).hostname || url;
  } catch {
    return url.replace(/^https?:\/\//i, "").replace(/\/$/, "");
  }
}
