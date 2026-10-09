import { useState } from "react";

import { Checkbox } from "@/components/ui/checkbox";
import { cn } from "@/lib/utils";
import { CAPABILITY_DESCRIPTIONS, capabilityLabel } from "./capabilities";

// The one write row, design v7 S2.1 and S2.2: "the write box and label"
// shared verbatim between the wizard's step 4 and the consent screen, so the
// two surfaces cannot drift into describing two different things by the same
// name.
//
// VISIBLY DISTINCT, NEVER PRE-TICKED, IN NO PRESET. This is its own bordered
// section, never a row inside the read checkboxes: KNOWN_CAPABILITIES.map in
// connect-wizard.tsx and the consent screen's capability list both render
// only the READ rows and stop, and this component renders the one row
// neither of them may include.
//
// THE COPY IS THE STATE AT MERGE, WHEN NO HOSTING CACHE IS CONFIRMED (design
// v7 S2 rules). Every hosting-cache sentence here matches CAPABILITY_DESCRIPTIONS'
// "mcp.cache.purge" entry, which is the one place that sentence is spelled --
// this component renders it, rather than typing a second copy of it.

const CACHE_CAPABILITY = "mcp.cache.purge" as const;

/**
 * The closing line design v7 S2.1 calls out for verbatim restoration:
 * "There is no group called 'run PHP' ... and no tier that removes the
 * requirement." It is exported so a regression test can assert the exact
 * string rather than a paraphrase of it.
 */
export const NO_UNGATED_WRITE_TIER_NOTE =
  "There is no group called “run PHP”, “WP-CLI”, “shell” or “open an arbitrary file”. " +
  "There is no group that can be granted without approval on the write side, and no " +
  "tier that removes the requirement.";

export interface CachePurgeCapabilityBoxProps {
  readonly checked: boolean;
  readonly onChange: (checked: boolean) => void;
  readonly disabled?: boolean;
  /**
   * False when the server did not offer the cache clear to this app as a
   * request. The row is then disabled and shown clear, so what the box shows
   * ticked is what the approval sends. Default true.
   */
  readonly offered?: boolean;
}

/**
 * The bordered "Changes it can ask for" box: one checkbox, its full
 * description, and a disclosure of which hosting caches are cleared and
 * which are skipped (design v7 S2.1's "[Which hosting caches it clears and
 * skips]" link).
 */
export function CachePurgeCapabilityBox({
  checked,
  onChange,
  disabled,
  offered = true,
}: CachePurgeCapabilityBoxProps) {
  const [showDisclosure, setShowDisclosure] = useState(false);
  const locked = disabled === true || !offered;

  return (
    <div
      data-testid="cache-purge-capability-box"
      className="rounded-lg border border-[var(--color-border)] p-3"
    >
      <p className="mb-2 text-xs font-medium uppercase tracking-wide text-[var(--color-muted-foreground)]">
        Changes it can ask for
      </p>
      <label
        className={cn(
          "flex items-start gap-2 rounded-md border border-[var(--color-border)] p-2 text-sm",
          locked && "cursor-not-allowed opacity-70",
        )}
      >
        <Checkbox
          className="mt-0.5"
          checked={checked && offered}
          disabled={locked}
          onChange={(e) => onChange(e.target.checked)}
        />
        <span>
          <span className="block font-medium text-[var(--color-foreground)]">
            {capabilityLabel(CACHE_CAPABILITY)}
          </span>
          <span className="block text-xs text-[var(--color-muted-foreground)]">
            {CAPABILITY_DESCRIPTIONS[CACHE_CAPABILITY]}
          </span>
          {!offered ? (
            <span
              data-testid="cache-purge-not-offered"
              className="mt-1 block text-xs font-medium text-[var(--color-muted-foreground)]"
            >
              Not requested by this app
            </span>
          ) : null}
          <button
            type="button"
            data-testid="cache-purge-disclosure-toggle"
            onClick={(e) => {
              e.preventDefault();
              setShowDisclosure((s) => !s);
            }}
            className="mt-1 text-xs font-medium text-[var(--color-primary)] underline underline-offset-2"
          >
            {showDisclosure ? "Hide" : "Which hosting caches it clears and skips"}
          </button>
          {showDisclosure ? (
            <span
              data-testid="cache-purge-disclosure"
              className="mt-1 block text-xs text-[var(--color-muted-foreground)]"
            >
              No hosting cache is confirmed yet, so every one of them is skipped. WPMgr
              clears only the pages it caches itself.
            </span>
          ) : null}
        </span>
      </label>
      <p className="mt-2 text-xs text-[var(--color-muted-foreground)]">
        {NO_UNGATED_WRITE_TIER_NOTE}
      </p>
    </div>
  );
}
