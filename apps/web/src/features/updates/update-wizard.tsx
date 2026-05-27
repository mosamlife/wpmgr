import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { useCreateUpdateRun } from "@/features/updates/use-updates";
import type { Site, UpdateItem, UpdateRunCreate } from "@wpmgr/api";

// Bulk update wizard (operator+). Opened from the sites list once the user has
// either selected one or more sites (by id) OR entered a tag. It collects:
//   - whether to update WordPress core
//   - a set of plugin/theme slugs (seeded from the selected sites' reported
//     components, each defaulting to version "latest")
//   - a dry-run toggle (default ON so the first submit is a safe preview)
//   - an optional schedule time
// Submitting POSTs /api/v1/updates and navigates to the run detail page.

export type WizardTarget =
  | { kind: "sites"; siteIds: string[] }
  | { kind: "tag"; tag: string };

interface ComponentOption {
  type: "plugin" | "theme";
  slug: string;
  label: string;
}

/** Build a de-duplicated, sorted list of plugin/theme options from sites. */
function componentOptions(sites: Site[]): ComponentOption[] {
  const seen = new Map<string, ComponentOption>();
  for (const site of sites) {
    for (const plugin of site.components?.plugins ?? []) {
      const key = `plugin:${plugin.slug}`;
      if (!seen.has(key)) {
        seen.set(key, {
          type: "plugin",
          slug: plugin.slug,
          label: plugin.name ?? plugin.slug,
        });
      }
    }
    for (const theme of site.components?.themes ?? []) {
      const key = `theme:${theme.slug}`;
      if (!seen.has(key)) {
        seen.set(key, {
          type: "theme",
          slug: theme.slug,
          label: theme.name ?? theme.slug,
        });
      }
    }
  }
  return Array.from(seen.values()).sort((a, b) =>
    a.label.localeCompare(b.label),
  );
}

/** Stable identity for a target, used to remount (reset) the form on open. */
function targetKey(target: WizardTarget | null): string {
  if (!target) return "none";
  return target.kind === "sites"
    ? `sites:${[...target.siteIds].sort().join(",")}`
    : `tag:${target.tag}`;
}

export function UpdateWizard({
  open,
  onClose,
  target,
  sites,
}: {
  open: boolean;
  onClose: () => void;
  target: WizardTarget | null;
  // Sites used to seed plugin/theme options (the currently selected/visible
  // sites). May be empty — the user can still add slugs manually.
  sites: Site[];
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);

  // Sync native <dialog> open state with React state (no ref touch in render).
  useEffect(() => {
    const el = dialogRef.current;
    if (!el) return;
    if (open && !el.open) el.showModal();
    if (!open && el.open) el.close();
  }, [open]);

  return (
    <dialog
      ref={dialogRef}
      onClose={onClose}
      aria-labelledby="update-wizard-title"
      className="m-auto w-full max-w-lg rounded-xl border border-[var(--color-border)] bg-[var(--color-background)] p-6 text-[var(--color-foreground)] backdrop:bg-black/50"
    >
      {/* Keying on (open + target) remounts the form so its local state resets
          cleanly each time the wizard is opened — no setState-in-effect. */}
      {open && target ? (
        <WizardForm
          key={targetKey(target)}
          target={target}
          sites={sites}
          onClose={onClose}
        />
      ) : null}
    </dialog>
  );
}

function WizardForm({
  target,
  sites,
  onClose,
}: {
  target: WizardTarget;
  sites: Site[];
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const create = useCreateUpdateRun();

  const [updateCore, setUpdateCore] = useState(false);
  const [selectedSlugs, setSelectedSlugs] = useState<Set<string>>(new Set());
  const [manualSlugs, setManualSlugs] = useState("");
  const [dryRun, setDryRun] = useState(true);
  const [scheduleAt, setScheduleAt] = useState("");

  const options = useMemo(() => componentOptions(sites), [sites]);

  function toggleSlug(key: string) {
    setSelectedSlugs((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }

  function buildItems(): UpdateItem[] {
    const items: UpdateItem[] = [];
    if (updateCore) items.push({ type: "core", version: "latest" });
    for (const key of selectedSlugs) {
      const opt = options.find((o) => `${o.type}:${o.slug}` === key);
      if (opt) items.push({ type: opt.type, slug: opt.slug, version: "latest" });
    }
    // Manual plugin slugs (comma/space separated) default to plugin/latest.
    for (const raw of manualSlugs.split(/[,\s]+/)) {
      const slug = raw.trim();
      if (!slug) continue;
      if (items.some((i) => i.type === "plugin" && i.slug === slug)) continue;
      items.push({ type: "plugin", slug, version: "latest" });
    }
    return items;
  }

  const items = buildItems();
  const targetDescribed =
    target.kind === "sites"
      ? `${target.siteIds.length} selected site${target.siteIds.length === 1 ? "" : "s"}`
      : `sites tagged “${target.tag}”`;

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (items.length === 0) return;

    const scheduleIso = scheduleAt
      ? new Date(scheduleAt).toISOString()
      : undefined;

    const body: UpdateRunCreate = {
      ...(target.kind === "sites"
        ? { site_ids: target.siteIds }
        : { tag: target.tag }),
      items,
      dry_run: dryRun,
      ...(scheduleIso ? { schedule_at: scheduleIso } : {}),
    };

    const run = await create.mutateAsync(body, { onError: () => {} });
    onClose();
    void navigate({ to: "/updates/$runId", params: { runId: run.id } });
  }

  return (
    <form onSubmit={(e) => void onSubmit(e)} noValidate className="space-y-5">
      <div>
        <h2 id="update-wizard-title" className="text-lg font-semibold">
          Bulk update
        </h2>
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Targeting {targetDescribed}. Choose what to update; a dry run previews
          changes without modifying any site.
        </p>
      </div>

      <fieldset className="space-y-3">
        <legend className="text-sm font-medium">What to update</legend>

        <label className="flex items-center gap-2 text-sm">
          <Checkbox
            checked={updateCore}
            onChange={(e) => setUpdateCore(e.target.checked)}
          />
          WordPress core (to latest)
        </label>

        {options.length > 0 ? (
          <div className="space-y-1">
            <p className="text-xs text-[var(--color-muted-foreground)]">
              Plugins &amp; themes detected on the selected sites:
            </p>
            <ul className="max-h-44 space-y-1 overflow-y-auto rounded-md border border-[var(--color-border)] p-2">
              {options.map((opt) => {
                const key = `${opt.type}:${opt.slug}`;
                const id = `opt-${key}`;
                return (
                  <li key={key}>
                    <label
                      htmlFor={id}
                      className="flex items-center gap-2 text-sm"
                    >
                      <Checkbox
                        id={id}
                        checked={selectedSlugs.has(key)}
                        onChange={() => toggleSlug(key)}
                      />
                      <span className="font-medium">{opt.label}</span>
                      <span className="text-xs text-[var(--color-muted-foreground)] capitalize">
                        {opt.type}
                      </span>
                    </label>
                  </li>
                );
              })}
            </ul>
          </div>
        ) : null}

        <div className="space-y-1">
          <Label htmlFor="manual-slugs">Additional plugin slugs</Label>
          <Input
            id="manual-slugs"
            placeholder="akismet, jetpack"
            value={manualSlugs}
            onChange={(e) => setManualSlugs(e.target.value)}
            aria-describedby="manual-slugs-hint"
          />
          <p
            id="manual-slugs-hint"
            className="text-xs text-[var(--color-muted-foreground)]"
          >
            Comma- or space-separated. Each updates to its latest version.
          </p>
        </div>
      </fieldset>

      <div className="space-y-3 rounded-md border border-[var(--color-border)] p-3">
        <label className="flex items-center gap-2 text-sm font-medium">
          <Checkbox
            checked={dryRun}
            onChange={(e) => setDryRun(e.target.checked)}
          />
          Dry run (preview only — no sites are modified)
        </label>

        <div className="space-y-1">
          <Label htmlFor="schedule-at">Schedule (optional)</Label>
          <Input
            id="schedule-at"
            type="datetime-local"
            value={scheduleAt}
            onChange={(e) => setScheduleAt(e.target.value)}
            className="w-60"
          />
          <p className="text-xs text-[var(--color-muted-foreground)]">
            Leave empty to run immediately.
          </p>
        </div>
      </div>

      {items.length === 0 ? (
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Select at least one thing to update.
        </p>
      ) : (
        <p className="text-sm text-[var(--color-muted-foreground)]">
          {items.length} item{items.length === 1 ? "" : "s"} will be{" "}
          {dryRun ? "previewed" : "updated"}.
        </p>
      )}

      {create.isError ? (
        <p role="alert" className="text-sm text-[var(--color-destructive)]">
          {create.error.message}
        </p>
      ) : null}

      <div className="flex justify-end gap-2">
        <Button
          type="button"
          variant="outline"
          onClick={onClose}
          disabled={create.isPending}
        >
          Cancel
        </Button>
        <Button
          type="submit"
          disabled={create.isPending || items.length === 0}
        >
          {create.isPending
            ? "Starting…"
            : dryRun
              ? "Preview update"
              : "Start update"}
        </Button>
      </div>
    </form>
  );
}
