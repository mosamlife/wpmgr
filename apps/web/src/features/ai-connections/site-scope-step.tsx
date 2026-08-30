import { useMemo, useState } from "react";
import { X } from "lucide-react";

import { Checkbox } from "@/components/ui/checkbox";
import { SegmentedControl } from "@/components/ui/segmented-control";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

import {
  describeSiteScope,
  resolveSiteScope,
  type FleetSnapshot,
  type ResolvedSiteScope,
  type SiteScopeMode,
} from "@/features/mcp-consent/site-scope";
import {
  SITE_STEP_MODES,
  canLeaveSiteStep,
  emptyTokenFieldLabel,
  excludedSitesLabel,
  fleetTotal,
  scopeCountLabel,
  scopeTokenLabel,
  siteStepBlockedReason,
} from "./site-step";

// Step 3 of the connection wizard: which sites this connection may touch.
//
// WHY IT SITS BEFORE CAPABILITIES. The wireframe states the ordering as a
// decision rather than a layout: "Chosen before capabilities, on purpose". A
// capability list is meaningless until its blast radius is fixed, and asking
// "what may it do" before "to what" produces a screen where every answer has to
// be revised once the second question is answered.
//
// EVERY SENTENCE WITH A NUMBER IN IT COMES FROM site-step.ts OR site-scope.ts.
// Nothing here computes a count inline, so there is exactly one place where a
// partial page could be rendered as a whole fleet, and it is a pure function
// with tests around it.

export interface SiteScopeSelection {
  readonly mode: SiteScopeMode;
  readonly tagNames: readonly string[];
  readonly siteIds: readonly string[];
}

export interface SiteScopeStepProps {
  readonly selection: SiteScopeSelection;
  readonly onSelectionChange: (next: SiteScopeSelection) => void;
  /**
   * What we loaded of the fleet, or null when the load has not finished or
   * failed. NULL IS NOT AN EMPTY SNAPSHOT. See site-scope.ts.
   */
  readonly fleet: FleetSnapshot | null;
  /**
   * The tag registry, or null when we could not load it. Null is not an empty
   * registry: one is a fact about the organisation, the other about our request.
   */
  readonly tags: readonly { readonly id: string; readonly name: string }[] | null;
  readonly tagsBySiteId: Readonly<Record<string, readonly string[]>>;
  readonly sitesLoading: boolean;
}

export function SiteScopeStep({
  selection,
  onSelectionChange,
  fleet,
  tags,
  tagsBySiteId,
  sitesLoading,
}: SiteScopeStepProps) {
  const [pickerOpen, setPickerOpen] = useState(false);
  const { mode, tagNames, siteIds } = selection;

  const scope: ResolvedSiteScope = useMemo(
    () =>
      resolveSiteScope({
        mode,
        selectedTagNames: tagNames,
        selectedSiteIds: siteIds,
        fleet,
        tagsBySiteId,
        sitesLoading,
      }),
    [mode, tagNames, siteIds, fleet, tagsBySiteId, sitesLoading],
  );

  const total = fleetTotal(fleet);
  const blocked = siteStepBlockedReason(scope);
  const excluded = excludedSitesLabel(scope, total);

  function setMode(next: SiteScopeMode) {
    // The selections are kept rather than cleared when the mode changes. They
    // are separate fields on the wire (scope_tag_ids and scope_site_ids) and
    // resolveSiteScope only ever reads the one belonging to the current mode,
    // so a mode flipped by accident does not throw away the other answer.
    onSelectionChange({ ...selection, mode: next });
    setPickerOpen(false);
  }

  function toggleTag(name: string) {
    onSelectionChange({
      ...selection,
      tagNames: tagNames.includes(name)
        ? tagNames.filter((t) => t !== name)
        : [...tagNames, name],
    });
  }

  function toggleSite(id: string) {
    onSelectionChange({
      ...selection,
      siteIds: siteIds.includes(id) ? siteIds.filter((s) => s !== id) : [...siteIds, id],
    });
  }

  const tokens: readonly { key: string; label: string; remove: () => void }[] =
    mode === "tags"
      ? tagNames.map((name) => ({
          key: name,
          label: scopeTokenLabel("tags", name),
          remove: () => toggleTag(name),
        }))
      : mode === "list"
        ? siteIds.map((id) => ({
            key: id,
            label: scopeTokenLabel(
              "list",
              fleet?.sites.find((s) => s.id === id)?.name ?? id,
            ),
            remove: () => toggleSite(id),
          }))
        : [];

  return (
    <div className="space-y-3">
      <SegmentedControl<SiteScopeMode>
        aria-label="Site scope"
        value={mode}
        onChange={setMode}
        options={SITE_STEP_MODES.map((m) => ({ value: m.value, label: m.label }))}
      />

      {/* The L state. A tag being resolved is a real wait, and the wireframe
          gives it skeleton rows rather than a spinner, because what is arriving
          is a list. */}
      {scope.kind === "unresolved" && scope.because === "loading" ? (
        <div data-testid="site-step-resolving" className="space-y-2">
          <Skeleton className="h-4 w-3/4" />
          <Skeleton className="h-4 w-1/2" />
          <Skeleton className="h-4 w-2/3" />
        </div>
      ) : null}

      {mode !== "all" ? (
        <div>
          <div
            data-testid="site-step-tokenfield"
            className="flex flex-wrap items-center gap-2 rounded-md border border-[var(--color-border)] p-2"
          >
            {tokens.length === 0 ? (
              <span className="px-1 text-xs text-[var(--color-muted-foreground)]">
                {emptyTokenFieldLabel(mode)}
              </span>
            ) : (
              tokens.map((t) => (
                <span
                  key={t.key}
                  className="inline-flex items-center gap-1 rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 px-2 py-0.5 text-xs text-[var(--color-foreground)]"
                >
                  <span className="font-mono">{t.label}</span>
                  <button
                    type="button"
                    onClick={t.remove}
                    aria-label={`Remove ${t.label}`}
                    className="rounded-sm text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
                  >
                    <X aria-hidden="true" className="size-3" />
                  </button>
                </span>
              ))
            )}
            <button
              type="button"
              onClick={() => setPickerOpen((open) => !open)}
              aria-expanded={pickerOpen}
              className="ml-auto rounded-md border border-[var(--color-border)] px-2 py-1 text-xs text-[var(--color-foreground)] hover:bg-[var(--color-accent)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
            >
              {/* Named for what the picker below actually lists. The wireframe
                  writes "Add sites" on both modes; a control that opens a list
                  of tags and calls them sites is the kind of small lie this
                  screen exists to avoid. */}
              {mode === "tags" ? "+ Add tags" : "+ Add sites"}
            </button>
          </div>

          {pickerOpen ? (
            <Picker
              mode={mode}
              fleet={fleet}
              tags={tags}
              tagNames={tagNames}
              siteIds={siteIds}
              sitesLoading={sitesLoading}
              onToggleTag={toggleTag}
              onToggleSite={toggleSite}
            />
          ) : null}
        </div>
      ) : null}

      <p
        data-testid="site-step-count"
        className={cn(
          "text-sm font-medium",
          canLeaveSiteStep(scope)
            ? "text-[var(--color-foreground)]"
            : "text-[var(--color-destructive)]",
        )}
      >
        {scopeCountLabel(scope, total)}
      </p>

      {/* THE EMPTY STATE, AND THE ONE SENTENCE THAT MAKES IT A STATE RATHER THAN
          A FAILURE. Nothing here is styled as an error and nothing blocks on it:
          an empty scope is a credential that reaches nothing yet, which is a
          thing an operator is allowed to want. */}
      {scope.kind === "none" && scope.because === "no-selection" ? (
        <p
          data-testid="site-step-empty"
          className="text-sm text-[var(--color-muted-foreground)]"
        >
          A connection with an empty scope can read nothing and propose nothing. That is a
          working state, not an error. It is how you mint a credential now and decide its
          reach later.
        </p>
      ) : (
        <p
          data-testid="site-step-summary"
          className="text-sm text-[var(--color-muted-foreground)]"
        >
          {describeSiteScope(scope)}
        </p>
      )}

      {blocked !== null ? (
        <p
          role="alert"
          data-testid="site-step-blocked"
          className="rounded-md border border-[var(--color-destructive)]/30 p-3 text-sm text-[var(--color-destructive)]"
        >
          {blocked}
        </p>
      ) : null}

      {excluded !== null ? (
        <p data-testid="site-step-excluded" className="text-xs text-[var(--color-muted-foreground)]">
          {excluded}
        </p>
      ) : null}

      {mode === "tags" ? (
        <p className="text-xs text-[var(--color-muted-foreground)]">
          A tag is resolved to a site list at every request, not frozen here. Add a site to
          the tag next month and this connection reaches it.
        </p>
      ) : null}

      <p className="text-xs text-[var(--color-muted-foreground)]">
        A site outside this scope is not greyed out with a reason when the model asks for
        it. It is not listed at all. Telling a caller that a site exists but is off limits
        is itself a disclosure.
      </p>
    </div>
  );
}

/**
 * The add panel.
 *
 * A FAILED REGISTRY AND AN EMPTY ONE GET DIFFERENT PANELS. `tags === null` is
 * our request failing and says so; `tags === []` is a claim about the
 * organisation and is only made when we actually read the registry. Same split
 * for the fleet one branch down.
 *
 * AND A LOAD IN PROGRESS IS NEITHER. `fleet === null` covers both "we asked and
 * it failed" and "we have not finished asking", so the failure panel used to
 * render over a read that was still running -- a load in progress stated as a
 * fact about the world, on the one panel whose job is to be honest about what
 * we know. `sitesLoading` splits them, and it is checked FIRST because the
 * failure copy is only true once there is nothing still in flight to be waiting
 * on.
 */
function Picker({
  mode,
  fleet,
  tags,
  tagNames,
  siteIds,
  sitesLoading,
  onToggleTag,
  onToggleSite,
}: {
  mode: SiteScopeMode;
  fleet: FleetSnapshot | null;
  tags: readonly { readonly id: string; readonly name: string }[] | null;
  tagNames: readonly string[];
  siteIds: readonly string[];
  sitesLoading: boolean;
  onToggleTag: (name: string) => void;
  onToggleSite: (id: string) => void;
}) {
  if (mode === "tags") {
    if (tags === null) {
      return (
        <PickerNote testId="site-step-tags-failed" tone="error">
          We could not load this organisation&apos;s tags, so there is nothing to pick
          from. That is not the same as having no tags.
        </PickerNote>
      );
    }
    if (tags.length === 0) {
      return (
        <PickerNote testId="site-step-tags-empty" tone="muted">
          This organisation has no tags yet. Create one on a site, or pick named sites
          instead.
        </PickerNote>
      );
    }
    return (
      <div data-testid="site-step-picker" className="mt-2 max-h-56 space-y-2 overflow-y-auto rounded-md border border-[var(--color-border)] p-2">
        {tags.map((tag) => (
          <label key={tag.id} className="flex items-start gap-2 text-sm">
            <Checkbox
              checked={tagNames.includes(tag.name)}
              onChange={() => onToggleTag(tag.name)}
            />
            <span className="font-mono text-xs">{scopeTokenLabel("tags", tag.name)}</span>
          </label>
        ))}
      </div>
    );
  }

  if (fleet === null) {
    if (sitesLoading) {
      return (
        <PickerNote testId="site-step-sites-loading" tone="muted">
          Still reading this organisation&apos;s sites. Nothing is missing yet.
        </PickerNote>
      );
    }
    return (
      <PickerNote testId="site-step-sites-failed" tone="error">
        We could not load this organisation&apos;s sites, so there is nothing to pick from.
        That is not the same as having no sites.
      </PickerNote>
    );
  }

  return (
    <div data-testid="site-step-picker" className="mt-2 space-y-2">
      {/* The picker is where truncation costs CHOICE rather than accuracy: the
          grant is exactly what was ticked, but a site that never rendered
          could not be ticked. Said here, beside the list it applies to. */}
      {!fleet.complete ? (
        <p
          role="alert"
          data-testid="site-step-picker-truncated"
          className="rounded-md border border-[var(--color-destructive)]/30 p-2 text-xs text-[var(--color-destructive)]"
        >
          This organisation has more sites than we can list here, so the choices below are
          not all of them. Anything you do not see is not covered.
        </p>
      ) : null}
      {fleet.sites.length === 0 ? (
        <PickerNote testId="site-step-sites-empty" tone="muted">
          This organisation has no sites yet, so there is nothing to scope this connection
          to.
        </PickerNote>
      ) : (
        <div className="max-h-56 space-y-2 overflow-y-auto rounded-md border border-[var(--color-border)] p-2">
          {fleet.sites.map((site) => (
            <label key={site.id} className="flex items-start gap-2 text-sm">
              <Checkbox
                checked={siteIds.includes(site.id)}
                onChange={() => onToggleSite(site.id)}
              />
              <span>
                <span className="font-medium">{site.name}</span>{" "}
                <span className="text-[var(--color-muted-foreground)]">{site.url}</span>
              </span>
            </label>
          ))}
        </div>
      )}
    </div>
  );
}

function PickerNote({
  testId,
  tone,
  children,
}: {
  testId: string;
  tone: "error" | "muted";
  children: React.ReactNode;
}) {
  return (
    <p
      role={tone === "error" ? "alert" : undefined}
      data-testid={testId}
      className={cn(
        "mt-2 rounded-md border p-2 text-xs",
        tone === "error"
          ? "border-[var(--color-destructive)]/30 text-[var(--color-destructive)]"
          : "border-[var(--color-border)] text-[var(--color-muted-foreground)]",
      )}
    >
      {children}
    </p>
  );
}
