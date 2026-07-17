import { useMemo, useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Check, Minus, Plus } from "lucide-react";
import type { Site, SiteTag } from "@wpmgr/api";

import {
  Command,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";
import { resolveTagDot } from "@/lib/tag-color";
import { useTags, useCreateTag } from "@/features/tags/use-tags";
import { getCachedSiteTags } from "@/features/sites/use-sites";
import { useSiteTagToggle } from "@/features/sites/use-site-tag-toggle";

// GH #230 "rich tags" — the tag picker. Shared between:
//   - single-site mode (SiteTagPickerPopover below): each toggle applies
//     OPTIMISTICALLY via the existing set-site-tags mutation; the popover
//     stays open across toggles.
//   - bulk tri-state mode (features/sites/bulk-action-drawer.tsx's
//     TagEditPanel): toggles only mutate PENDING local state; the caller
//     applies everything in one POST /tags/bulk-apply on "Apply".
//
// Keyboard / ARIA (APG combobox): cmdk keeps DOM focus in <CommandInput> the
// whole time and moves the "active" option via `aria-activedescendant` on
// ArrowUp/ArrowDown/Home/End (see components/ui/command.tsx). Enter dispatches
// a select on the active option WITHOUT closing anything — closing is fully
// owned by the caller (a wrapping Popover, for single-site mode). Every
// option additionally carries `aria-checked` reflecting OUR domain state
// (assigned / not assigned / mixed across a bulk selection), so this reads to
// assistive tech as a checkable option list layered on the standard combobox
// listbox pattern that cmdk already implements. A polite aria-live region
// announces every toggle ("{name} added" / "{name} removed").

export type TagPickerState = "checked" | "unchecked" | "mixed";

export interface TagPickerProps {
  /** Current UI state for a given registry tag. */
  getState: (tag: SiteTag) => TagPickerState;
  /**
   * Toggle/cycle a tag. Single-site mode only ever sees "checked"/"unchecked"
   * (a plain flip). Bulk mode cycles mixed -> checked -> unchecked -> checked…
   */
  onToggle: (tag: SiteTag) => void;
  /**
   * Create a new tag from the search query. Resolves to the created tag
   * (already registered server-side); the picker then calls `onToggle` on it
   * so the caller applies/marks it exactly like any other toggle.
   */
  onCreate: (name: string) => Promise<SiteTag>;
  placeholder?: string;
  className?: string;
}

export function TagPicker({
  getState,
  onToggle,
  onCreate,
  placeholder = "Search or create tags…",
  className,
}: TagPickerProps) {
  const { data: tags } = useTags();
  const [query, setQuery] = useState("");
  const [announcement, setAnnouncement] = useState("");
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  // "assigned-first pinned AT OPEN TIME (no live reorder)" — freeze the set of
  // tag ids that were already checked/mixed the FIRST time the registry data
  // is available, and never recompute it afterward, so toggling a tag mid-
  // session does not make rows jump around under the operator's cursor.
  // Stored in state (not a ref) and set conditionally during render — the
  // same "adjust state during render" idiom used by site-tags-editor.tsx's
  // server-key reset — because this codebase's stricter React Compiler lint
  // config forbids reading/writing a ref's `.current` during render.
  const [pinned, setPinned] = useState<Set<string> | null>(null);
  const all = useMemo(() => tags ?? [], [tags]);
  if (pinned === null && tags !== undefined) {
    setPinned(new Set(tags.filter((t) => getState(t) !== "unchecked").map((t) => t.id)));
  }

  const ordered = useMemo(() => {
    const pinnedIds = pinned ?? new Set<string>();
    const byName = (a: SiteTag, b: SiteTag) =>
      a.name.localeCompare(b.name, undefined, { sensitivity: "base" });
    const assigned = all.filter((t) => pinnedIds.has(t.id)).sort(byName);
    const rest = all.filter((t) => !pinnedIds.has(t.id)).sort(byName);
    return [...assigned, ...rest];
  }, [all, pinned]);

  const trimmed = query.trim();
  const q = trimmed.toLowerCase();
  const filtered = q ? ordered.filter((t) => t.name.toLowerCase().includes(q)) : ordered;

  const hasExactMatch = trimmed.length > 0 && all.some((t) => t.name.toLowerCase() === q);
  const showCreateRow = trimmed.length > 0 && !hasExactMatch;

  function handleToggle(tag: SiteTag) {
    const prev = getState(tag);
    onToggle(tag);
    setAnnouncement(`${tag.name} ${prev === "checked" ? "removed" : "added"}`);
  }

  async function handleCreate() {
    if (!trimmed || creating) return;
    setCreating(true);
    setCreateError(null);
    try {
      const created = await onCreate(trimmed);
      setPinned((prev) => new Set(prev).add(created.id));
      onToggle(created);
      setAnnouncement(`${created.name} added`);
      setQuery("");
    } catch (err) {
      setCreateError(err instanceof Error ? err.message : "Could not create tag.");
    } finally {
      setCreating(false);
    }
  }

  return (
    <Command className={className} shouldFilter={false}>
      <CommandInput
        value={query}
        onValueChange={setQuery}
        placeholder={placeholder}
        aria-label="Search or create tags"
      />
      <CommandList aria-multiselectable="true">
        {filtered.length === 0 ? (
          <div
            role="presentation"
            className="px-3 py-6 text-center text-sm text-muted-foreground"
          >
            {trimmed ? "No matching tags." : "No tags yet."}
          </div>
        ) : (
          <CommandGroup>
            {filtered.map((tag) => {
              const state = getState(tag);
              const dot = resolveTagDot(tag);
              return (
                <CommandItem
                  key={tag.id}
                  value={tag.id}
                  keywords={[tag.name]}
                  aria-checked={state === "mixed" ? "mixed" : state === "checked"}
                  onSelect={() => handleToggle(tag)}
                >
                  <StateGlyph state={state} />
                  <span
                    aria-hidden="true"
                    className={cn("size-2 shrink-0 rounded-full", dot.className)}
                    style={dot.style}
                  />
                  <span className="min-w-0 flex-1 truncate">{tag.name}</span>
                  <span className="shrink-0 text-xs tabular-nums text-muted-foreground">
                    {tag.usage_count}
                  </span>
                </CommandItem>
              );
            })}
          </CommandGroup>
        )}

        {showCreateRow ? (
          <CommandGroup>
            <CommandItem
              value={`__create__:${trimmed}`}
              onSelect={() => void handleCreate()}
              disabled={creating}
              className="text-primary aria-selected:text-primary"
            >
              <Plus aria-hidden="true" className="size-4 shrink-0" />
              <span className="min-w-0 flex-1 truncate">
                Create &quot;{trimmed}&quot;
              </span>
            </CommandItem>
          </CommandGroup>
        ) : null}

        {createError ? (
          <p role="alert" className="px-3 pb-2 text-xs text-destructive">
            {createError}
          </p>
        ) : null}
      </CommandList>

      <div aria-live="polite" role="status" className="sr-only">
        {announcement}
      </div>
    </Command>
  );
}

function StateGlyph({ state }: { state: TagPickerState }): ReactNode {
  if (state === "checked") {
    return <Check aria-hidden="true" className="size-4 shrink-0 text-primary" />;
  }
  if (state === "mixed") {
    return <Minus aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />;
  }
  // Fixed-width spacer so the color dot + name stay aligned across rows
  // regardless of which glyph (or none) is showing.
  return <span aria-hidden="true" className="size-4 shrink-0" />;
}

// ---------------------------------------------------------------------------
// SiteTagPickerPopover — single-site convenience wrapper
// ---------------------------------------------------------------------------

export interface SiteTagPickerPopoverProps {
  site: Site;
  trigger: ReactNode;
  align?: "start" | "end" | "center";
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
}

/**
 * Wraps <TagPicker> in a Popover wired to the single-site apply path. Each
 * toggle goes through `useSiteTagToggle`, which serializes per-site toggles
 * and always computes the replace-set from the FRESHEST cached tags (never
 * the captured `site` prop) — fixes the lost-update race where two rapid
 * toggles both read the same stale array and the second PUT silently
 * dropped the first (GH #230 adversarial-verify HIGH). The popover stays
 * open across toggles.
 */
export function SiteTagPickerPopover({
  site,
  trigger,
  align = "start",
  open,
  onOpenChange,
}: SiteTagPickerPopoverProps) {
  const queryClient = useQueryClient();
  const { toggleTag } = useSiteTagToggle(site);
  const createTag = useCreateTag();

  function getState(tag: SiteTag): TagPickerState {
    const current = getCachedSiteTags(queryClient, site.id, site.tags);
    return current.includes(tag.name) ? "checked" : "unchecked";
  }

  function applyToggle(tag: SiteTag) {
    toggleTag(tag.name);
  }

  async function handleCreate(name: string): Promise<SiteTag> {
    return createTag.mutateAsync({ name });
  }

  return (
    <Popover open={open} onOpenChange={onOpenChange}>
      <PopoverTrigger asChild>{trigger}</PopoverTrigger>
      <PopoverContent align={align} className="w-64 p-0">
        <TagPicker getState={getState} onToggle={applyToggle} onCreate={handleCreate} />
      </PopoverContent>
    </Popover>
  );
}
