import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { Search, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useSites } from "@/features/sites/use-sites";
import { SitesTable } from "@/features/sites/sites-table";
import { AddSiteDialog } from "@/features/sites/add-site-dialog";
import { useMe, canOperate } from "@/features/auth/use-auth";

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

  function applyFilter(e: React.FormEvent) {
    e.preventDefault();
    setAppliedTag(tagInput.trim());
  }

  function clearFilter() {
    setTagInput("");
    setAppliedTag("");
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
        <SitesTable sites={sites} />
      )}
    </section>
  );
}
