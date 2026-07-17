import { Plus, X } from "lucide-react";
import type { Site } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { TagChip } from "@/features/sites/tag-chip";
import { SiteTagPickerPopover } from "@/features/sites/tag-picker";
import { useSiteTagToggle } from "@/features/sites/use-site-tag-toggle";
import { useTagColorMap } from "@/features/tags/use-tag-color-map";

// GH #230 "rich tags" — inline editor for a site's tag set.
//
// Rewritten around the shared <TagPicker>: a persistent chip row (each chip
// carries an × remove) plus an always-visible "+ Add tag" trigger that opens
// the picker. Every toggle applies OPTIMISTICALLY via `useSiteTagToggle`
// (which serializes per-site toggles and always computes the replace-set
// from the freshest cached tags, not a captured prop — see that hook's doc
// for the lost-update race it fixes) — there is no local draft state and no
// Save/Discard step; tags commit instantly, independent of the rest of the
// settings form's StickySaveBar flow.
export function SiteTagsEditor({ site }: { site: Site }) {
  const { toggleTag } = useSiteTagToggle(site);
  const colorMap = useTagColorMap();

  function removeTag(name: string) {
    // Removing a chip is exactly "toggle that (present) tag off" — the same
    // race-safe path a picker toggle uses.
    toggleTag(name);
  }

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-1.5">
        {site.tags.length === 0 ? (
          <span className="text-sm text-muted-foreground">No tags</span>
        ) : (
          site.tags.map((name) => (
            <TagChip
              key={name}
              tag={{ name, color: colorMap.get(name) }}
              trailing={
                <button
                  type="button"
                  onClick={() => removeTag(name)}
                  aria-label={`Remove tag ${name}`}
                  className="ml-0.5 rounded-full p-0.5 hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1"
                >
                  <X aria-hidden="true" className="size-3" />
                </button>
              }
            />
          ))
        )}

        <SiteTagPickerPopover
          site={site}
          align="start"
          trigger={
            <Button type="button" variant="outline" size="sm" className="h-6 gap-1 px-2 text-xs">
              <Plus aria-hidden="true" className="size-3" />
              Add tag
            </Button>
          }
        />
      </div>
    </div>
  );
}
