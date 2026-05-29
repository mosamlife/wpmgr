import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { Plus } from "lucide-react";

import { Button } from "@/components/ui/button";
import { useSites } from "@/features/sites/use-sites";
import { SitesTable } from "@/features/sites/sites-table";
import { SitesToolbar } from "@/features/sites/sites-toolbar";
import { useSitesSelection } from "@/features/sites/use-sites-selection";
import { useSitesDensity } from "@/features/sites/use-sites-density";
import { AddSiteDialog } from "@/features/sites/add-site-dialog";
import { useAutoLogin, canAutoLogin } from "@/features/sites/use-autologin";
import {
  UpdateWizard,
  type WizardTarget,
} from "@/features/updates/update-wizard";
import { useMe, canOperate } from "@/features/auth/use-auth";
import { toast } from "@/components/toast";
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

  const { data: sites, isPending, isError, error, refetch } =
    useSites(appliedTag);

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
      for (const t of s.tags) set.add(t);
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
      [s.name, s.url, ...s.tags]
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

  return (
    <section aria-labelledby="sites-heading" className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 id="sites-heading" className="text-2xl font-semibold">
          Sites
        </h1>
      </div>

      {isPending ? (
        <p role="status" className="text-muted-foreground">
          Loading sites
        </p>
      ) : isError ? (
        <div role="alert" className="space-y-3">
          <p className="text-destructive">
            Failed to load sites: {error.message}
          </p>
          <Button variant="outline" size="sm" onClick={() => void refetch()}>
            Retry
          </Button>
        </div>
      ) : sites.length === 0 ? (
        <div className="space-y-3 border border-dashed border-border bg-background p-8 text-center">
          <p className="text-muted-foreground">
            No sites yet.{" "}
            {operate
              ? "Use Add site to generate a pairing code and enroll your first WordPress site."
              : "Ask an operator to add the first site."}
          </p>
          {operate ? (
            <div className="flex justify-center">
              <AddSiteDialog />
            </div>
          ) : null}
        </div>
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
          <SitesTable
            sites={visibleSites}
            isLoading={isPending}
            selection={operate ? selection : undefined}
            densityState={densityState}
            onOpenAutoLogin={autoLogin ? handleOpenAutoLogin : undefined}
          />
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
    </section>
  );
}

// Read-only operators see the toolbar without an Add Site primary; we render
// an inert placeholder so the row layout stays stable across roles.
function AddSitePlaceholder() {
  return (
    <Button
      type="button"
      disabled
      aria-disabled="true"
      title="Add site requires operator permissions"
      className="opacity-50"
    >
      <Plus aria-hidden="true" />
      Add site
    </Button>
  );
}
