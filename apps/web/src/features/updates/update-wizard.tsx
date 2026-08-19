import React, { useMemo, useState } from "react";
import { useNavigate } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { useCreateUpdateRun } from "@/features/updates/use-updates";
import { isAgentPluginComponent } from "@/features/updates/agent-plugin";
import {
  SCHEDULE_MAX_LEAD_DAYS,
  browserTimeZone,
  formatAbsolute,
  scheduleBounds,
  validateSchedule,
} from "@/features/updates/schedule";
import type { Site, UpdateItem, UpdateRunCreate } from "@wpmgr/api";

// Bulk update wizard (operator+). Opened from the sites list once the user has
// either selected one or more sites (by id) OR entered a tag. It collects:
//   - whether to update WordPress core
//   - a set of plugin/theme slugs (seeded from the selected sites' reported
//     components, each defaulting to version "latest")
//   - a dry-run toggle (default ON so the first submit is a safe preview)
//   - an optional schedule time
// Submitting POSTs /api/v1/updates and navigates to the run detail page.
//
// #98 UX fixes:
//   1. updateKind in WizardTarget drives the active tab/section (plugins,
//      themes, or core) so the split-button selection is honoured.
//   2. Components default to "has update" filter — only items with a
//      new_version reported by the agent are pre-checked. Items without an
//      update are hidden by default (operator can reveal via "Show all").
//   3. Plugins and themes are separated into tabs with their own lists.

export type WizardUpdateKind = "plugins" | "themes" | "core";

export type WizardTarget =
  | { kind: "sites"; siteIds: string[]; updateKind?: WizardUpdateKind }
  | { kind: "tag"; tag: string; updateKind?: WizardUpdateKind };

interface ComponentOption {
  type: "plugin" | "theme";
  slug: string;
  label: string;
  /** True when the agent reports a newer version is available. */
  hasUpdate: boolean;
}

/**
 * Normalize a version string for equality comparison: trim whitespace and
 * strip a single leading "v"/"V" (mirrors the backend's normalized-equality
 * check). Deliberately a simple string compare, not a semver parse; the
 * backend is the authority on real version semantics.
 */
function normalizeVersion(version: string): string {
  const trimmed = version.trim();
  return /^[vV]/.test(trimmed) ? trimmed.slice(1) : trimmed;
}

/**
 * True when `available_update` reports a version that actually differs from
 * what's installed. Guards against a phantom same-version entry (e.g. a
 * cached `1.5.1 -> 1.5.1` row) being treated as a real update (GH #211).
 * The backend is also being fixed to stop sending/persisting these, but this
 * keeps the wizard correct against already-cached data.
 */
function isRealUpdate(
  installedVersion: string | undefined,
  availableUpdate: { new_version: string } | undefined,
): boolean {
  if (!availableUpdate) return false;
  if (installedVersion === undefined) return true;
  return (
    normalizeVersion(availableUpdate.new_version) !==
    normalizeVersion(installedVersion)
  );
}

/** Build a de-duplicated, sorted list of plugin/theme options from sites. */
function componentOptions(sites: Site[]): ComponentOption[] {
  const seen = new Map<string, ComponentOption>();
  for (const site of sites) {
    for (const plugin of site.components?.plugins ?? []) {
      // Never offer the WPMgr agent's own plugin entry as an update target
      // (see agent-plugin.ts for why). Belt-and-braces: the control plane
      // already withholds the advisory at the source, so this should never
      // actually trigger, but it means a stale cache or hand-built payload
      // still can't get the agent into this list.
      if (isAgentPluginComponent(plugin.slug, plugin.name)) continue;
      const key = `plugin:${plugin.slug}`;
      if (!seen.has(key)) {
        seen.set(key, {
          type: "plugin",
          slug: plugin.slug,
          label: plugin.name ?? plugin.slug,
          hasUpdate: isRealUpdate(plugin.version, plugin.available_update),
        });
      } else {
        // If any site reports a real (different-version) update, mark the
        // option as having one.
        const existing = seen.get(key)!;
        if (
          isRealUpdate(plugin.version, plugin.available_update) &&
          !existing.hasUpdate
        ) {
          seen.set(key, { ...existing, hasUpdate: true });
        }
      }
    }
    for (const theme of site.components?.themes ?? []) {
      const key = `theme:${theme.slug}`;
      if (!seen.has(key)) {
        seen.set(key, {
          type: "theme",
          slug: theme.slug,
          label: theme.name ?? theme.slug,
          hasUpdate: isRealUpdate(theme.version, theme.available_update),
        });
      } else {
        const existing = seen.get(key)!;
        if (
          isRealUpdate(theme.version, theme.available_update) &&
          !existing.hasUpdate
        ) {
          seen.set(key, { ...existing, hasUpdate: true });
        }
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
  const kindSuffix = target.updateKind ?? "plugins";
  return target.kind === "sites"
    ? `sites:${[...target.siteIds].sort().join(",")}:${kindSuffix}`
    : `tag:${target.tag}:${kindSuffix}`;
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
  return (
    <Dialog open={open} onClose={onClose}>
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
    </Dialog>
  );
}

type ComponentTab = "plugins" | "themes";

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

  // The updateKind from the split-button drives the initial active tab.
  const initialTab: ComponentTab =
    target.updateKind === "themes" ? "themes" : "plugins";
  const initialUpdateCore = target.updateKind === "core";

  const [activeTab, setActiveTab] = useState<ComponentTab>(initialTab);
  const [updateCore, setUpdateCore] = useState(initialUpdateCore);
  const [selectedSlugs, setSelectedSlugs] = useState<Set<string>>(new Set());
  const [manualSlugs, setManualSlugs] = useState("");
  const [dryRun, setDryRun] = useState(true);
  const [scheduleAt, setScheduleAt] = useState("");
  // When true, only items with a reported update are shown.
  const [filterToUpdates, setFilterToUpdates] = useState(true);

  const options = useMemo(() => componentOptions(sites), [sites]);
  const pluginOptions = useMemo(
    () => options.filter((o) => o.type === "plugin"),
    [options],
  );
  const themeOptions = useMemo(
    () => options.filter((o) => o.type === "theme"),
    [options],
  );

  // Count items that have updates, for the filter label.
  const pluginsWithUpdate = useMemo(
    () => pluginOptions.filter((o) => o.hasUpdate).length,
    [pluginOptions],
  );
  const themesWithUpdate = useMemo(
    () => themeOptions.filter((o) => o.hasUpdate).length,
    [themeOptions],
  );

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

  // GH #463: the schedule bounds the control plane enforces, restated at the
  // point of typing. `scheduleProblem` is recomputed on every render rather
  // than memoised on `scheduleAt` alone, because it is time-relative: a value
  // that was two minutes out when it was typed is in the past by the time the
  // operator finishes choosing plugins, and the stale verdict would let it
  // through to a 422.
  const scheduleZone = browserTimeZone();
  const scheduleBoundsValue = scheduleBounds();
  const scheduleProblem = validateSchedule(scheduleAt);

  const targetDescribed =
    target.kind === "sites"
      ? `${target.siteIds.length} selected site${target.siteIds.length === 1 ? "" : "s"}`
      : `sites tagged "${target.tag}"`;

  const submitLabel = create.isPending
    ? "Starting..."
    : dryRun
      ? `Preview ${items.length} update${items.length === 1 ? "" : "s"}`
      : `Apply ${items.length} update${items.length === 1 ? "" : "s"}`;

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (items.length === 0) return;
    // GH #463: re-validate against the clock AT SUBMIT, not against the
    // verdict rendered earlier. The form is `noValidate`, so `min`/`max` on
    // the input are a picker hint only and never a gate.
    if (validateSchedule(scheduleAt)) return;

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

  // The visible list for the active tab, filtered when filterToUpdates is on.
  const visibleOptions = useMemo(() => {
    const list = activeTab === "plugins" ? pluginOptions : themeOptions;
    return filterToUpdates ? list.filter((o) => o.hasUpdate) : list;
  }, [activeTab, pluginOptions, themeOptions, filterToUpdates]);

  const totalWithUpdates =
    activeTab === "plugins" ? pluginsWithUpdate : themesWithUpdate;

  return (
    <DialogContent ariaLabelledBy="update-wizard-title" className="max-w-[560px]">
      <form onSubmit={(e) => void onSubmit(e)} noValidate>
        <DialogHeader>
          <DialogTitle id="update-wizard-title">Update sites</DialogTitle>
          <DialogDescription>
            Targeting {targetDescribed}. A dry run previews changes without
            modifying any site.
          </DialogDescription>
        </DialogHeader>

        <DialogBody>
          {/* WordPress core */}
          <div className="rounded-md border border-[var(--color-border)] p-3">
            <label className="flex items-center gap-2 text-sm font-medium">
              <Checkbox
                checked={updateCore}
                onChange={(e) => setUpdateCore(e.target.checked)}
              />
              WordPress core (update to latest)
            </label>
          </div>

          {/* Plugins / Themes tabs */}
          <div className="space-y-3">
            {/* Tab strip */}
            <div
              role="tablist"
              aria-label="Update target"
              className="inline-flex rounded-md border border-[var(--color-border)] bg-[var(--color-muted)] p-0.5"
            >
              {(["plugins", "themes"] as const).map((tab) => {
                const count =
                  tab === "plugins" ? pluginsWithUpdate : themesWithUpdate;
                return (
                  <button
                    key={tab}
                    type="button"
                    role="tab"
                    aria-selected={activeTab === tab}
                    onClick={() => setActiveTab(tab)}
                    className={`rounded-sm px-3 py-1.5 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${
                      activeTab === tab
                        ? "bg-[var(--color-background)] text-[var(--color-foreground)] shadow-sm"
                        : "text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)]"
                    }`}
                  >
                    {tab === "plugins" ? "Plugins" : "Themes"}
                    {count > 0 ? (
                      <span className="ml-1.5 rounded-sm bg-primary/10 px-1 text-[10px] font-semibold tabular-nums text-primary">
                        {count}
                      </span>
                    ) : null}
                  </button>
                );
              })}
            </div>

            {/* Filter toggle */}
            {(activeTab === "plugins" ? pluginOptions : themeOptions).length > 0 ? (
              <div className="flex items-center justify-between">
                <p className="text-xs text-[var(--color-muted-foreground)]">
                  {filterToUpdates
                    ? `Showing ${totalWithUpdates} with available update${totalWithUpdates === 1 ? "" : "s"}`
                    : `Showing all ${(activeTab === "plugins" ? pluginOptions : themeOptions).length}`}
                </p>
                <button
                  type="button"
                  onClick={() => setFilterToUpdates((v) => !v)}
                  className="text-xs text-primary underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                >
                  {filterToUpdates ? "Show all" : "Show only with updates"}
                </button>
              </div>
            ) : null}

            {/* Component list */}
            {visibleOptions.length > 0 ? (
              <ul
                role="tabpanel"
                className="max-h-44 space-y-1 overflow-y-auto rounded-md border border-[var(--color-border)] p-2"
              >
                {visibleOptions.map((opt) => {
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
                        {opt.hasUpdate ? (
                          <span className="rounded-sm bg-primary/10 px-1 text-[10px] font-semibold text-primary">
                            update
                          </span>
                        ) : (
                          <span className="text-xs text-[var(--color-muted-foreground)]">
                            up to date
                          </span>
                        )}
                      </label>
                    </li>
                  );
                })}
              </ul>
            ) : (
              <p className="text-sm text-[var(--color-muted-foreground)]">
                {filterToUpdates
                  ? `No ${activeTab} with available updates on the selected sites.`
                  : `No ${activeTab} detected on the selected sites.`}
              </p>
            )}

            {/* Manual slug entry */}
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
          </div>

          {/* Options */}
          <div className="space-y-3 rounded-md border border-[var(--color-border)] p-3">
            <label className="flex items-center gap-2 text-sm font-medium">
              <Checkbox
                checked={dryRun}
                onChange={(e) => setDryRun(e.target.checked)}
              />
              Dry run (preview only — no sites are modified)
            </label>

            <div className="space-y-1">
              {/* GH #463: the field now names the zone it is read in. A
                  `datetime-local` value is interpreted in the BROWSER's zone,
                  and that is the instant submitted below — so an operator in
                  London scheduling a Sydney site was previously shown neither
                  zone and had no way to tell which one they had picked. */}
              <Label htmlFor="schedule-at">
                Schedule (optional)
                {scheduleZone ? (
                  <span className="ml-1.5 font-normal text-[var(--color-muted-foreground)]">
                    in {scheduleZone}
                  </span>
                ) : null}
              </Label>
              <Input
                id="schedule-at"
                type="datetime-local"
                value={scheduleAt}
                onChange={(e) => setScheduleAt(e.target.value)}
                className="w-60"
                min={scheduleBoundsValue.min}
                max={scheduleBoundsValue.max}
                aria-invalid={scheduleProblem ? true : undefined}
                aria-describedby={
                  scheduleProblem ? "schedule-at-error" : "schedule-at-hint"
                }
              />
              {scheduleProblem ? (
                <p
                  id="schedule-at-error"
                  role="alert"
                  className="text-xs text-[var(--color-destructive)]"
                >
                  {scheduleProblem.message}
                </p>
              ) : (
                <p
                  id="schedule-at-hint"
                  className="text-xs text-[var(--color-muted-foreground)]"
                >
                  {scheduleAt
                    ? `Runs at ${formatAbsolute(new Date(scheduleAt).toISOString())}. Leave empty to run immediately.`
                    : `Leave empty to run immediately. Up to ${SCHEDULE_MAX_LEAD_DAYS} days ahead.`}
                </p>
              )}
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
        </DialogBody>

        <DialogFooter className="pt-2">
          <Button
            type="button"
            variant="outline"
            onClick={onClose}
            disabled={create.isPending}
          >
            Close
          </Button>
          <Button
            type="submit"
            disabled={
              create.isPending ||
              items.length === 0 ||
              // GH #463: a schedule the control plane would refuse
              // (`schedule_in_past` / `schedule_too_far`) never reaches it.
              scheduleProblem !== null
            }
          >
            {submitLabel}
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  );
}
