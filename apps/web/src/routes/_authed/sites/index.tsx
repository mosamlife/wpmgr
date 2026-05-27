import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { Search, X, RefreshCw } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useSites } from "@/features/sites/use-sites";
import {
  SitesTable,
  type SitesTableSelection,
} from "@/features/sites/sites-table";
import { AddSiteDialog } from "@/features/sites/add-site-dialog";
import {
  UpdateWizard,
  type WizardTarget,
} from "@/features/updates/update-wizard";
import { useMe, canOperate } from "@/features/auth/use-auth";
import type { Site } from "@wpmgr/api";

export const Route = createFileRoute("/_authed/sites/")({
  component: SitesPage,
});

function SitesPage() {
  const { data: me } = useMe();
  const operate = canOperate(me);

  // Controlled input vs. the applied filter: we only refetch when the user
  // submits, so the list does not thrash on every keystroke.
  const [tagInput, setTagInput] = useState("");
  const [appliedTag, setAppliedTag] = useState("");

  const { data: sites, isPending, isError, error, refetch } =
    useSites(appliedTag);

  // Multi-select state for the bulk update wizard (operator+ only).
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [wizardTarget, setWizardTarget] = useState<WizardTarget | null>(null);

  const selection: SitesTableSelection | undefined = operate
    ? {
        selected,
        onToggle: (id) =>
          setSelected((prev) => {
            const next = new Set(prev);
            if (next.has(id)) next.delete(id);
            else next.add(id);
            return next;
          }),
        onToggleAll: (ids, select) =>
          setSelected((prev) => {
            const next = new Set(prev);
            for (const id of ids) {
              if (select) next.add(id);
              else next.delete(id);
            }
            return next;
          }),
      }
    : undefined;

  const selectedSites: Site[] = (sites ?? []).filter((s) => selected.has(s.id));

  function applyFilter(e: React.FormEvent) {
    e.preventDefault();
    setAppliedTag(tagInput.trim());
  }

  function clearFilter() {
    setTagInput("");
    setAppliedTag("");
  }

  function openWizardForSelection() {
    setWizardTarget({ kind: "sites", siteIds: Array.from(selected) });
  }

  function openWizardForTag() {
    if (!appliedTag) return;
    setWizardTarget({ kind: "tag", tag: appliedTag });
  }

  return (
    <section aria-labelledby="sites-heading" className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 id="sites-heading" className="text-2xl font-semibold">
          Sites
        </h1>
        {operate ? <AddSiteDialog /> : null}
      </div>

      <form
        onSubmit={applyFilter}
        role="search"
        className="flex flex-wrap items-end gap-2"
      >
        <div className="space-y-1">
          <Label htmlFor="tag-filter">Filter by tag</Label>
          <Input
            id="tag-filter"
            value={tagInput}
            onChange={(e) => setTagInput(e.target.value)}
            placeholder="e.g. production"
            className="w-56"
          />
        </div>
        <Button type="submit" variant="outline" size="sm">
          <Search aria-hidden="true" />
          Filter
        </Button>
        {appliedTag ? (
          <Button type="button" variant="ghost" size="sm" onClick={clearFilter}>
            <X aria-hidden="true" />
            Clear
          </Button>
        ) : null}
      </form>

      {appliedTag ? (
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Showing sites tagged <strong>{appliedTag}</strong>.
        </p>
      ) : null}

      {operate ? (
        <div
          className="flex flex-wrap items-center gap-2"
          role="region"
          aria-label="Bulk actions"
        >
          <Button
            type="button"
            size="sm"
            onClick={openWizardForSelection}
            disabled={selected.size === 0}
          >
            <RefreshCw aria-hidden="true" />
            Update {selected.size} selected
          </Button>
          {appliedTag ? (
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={openWizardForTag}
            >
              Update all tagged “{appliedTag}”
            </Button>
          ) : null}
        </div>
      ) : null}

      {isPending ? (
        <p role="status" className="text-[var(--color-muted-foreground)]">
          Loading sites…
        </p>
      ) : isError ? (
        <div role="alert" className="space-y-3">
          <p className="text-[var(--color-destructive)]">
            Failed to load sites: {error.message}
          </p>
          <Button variant="outline" size="sm" onClick={() => void refetch()}>
            Retry
          </Button>
        </div>
      ) : sites.length === 0 ? (
        <div className="space-y-3 rounded-xl border border-dashed border-[var(--color-border)] p-8 text-center">
          {appliedTag ? (
            <p className="text-[var(--color-muted-foreground)]">
              No sites match the tag <strong>{appliedTag}</strong>.
            </p>
          ) : (
            <>
              <p className="text-[var(--color-muted-foreground)]">
                No sites yet.{" "}
                {operate
                  ? "Use “Add site” to generate a pairing code and enroll your first WordPress site."
                  : "Ask an operator to add the first site."}
              </p>
              {operate ? (
                <div className="flex justify-center">
                  <AddSiteDialog />
                </div>
              ) : null}
            </>
          )}
        </div>
      ) : (
        <SitesTable sites={sites} selection={selection} />
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
