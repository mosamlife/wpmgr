import { Check } from "lucide-react";

import { Checkbox } from "@/components/ui/checkbox";
import { cn } from "@/lib/utils";
import {
  CAPABILITY_DESCRIPTIONS,
  CONFERRABLE_READS,
  KNOWN_CAPABILITIES,
  capabilityKind,
  capabilityLabel,
  isAbilityCapability,
} from "./capabilities";
import { capabilityPresets, customNote, presetFor, withCapability } from "./capability-presets";

// The read picker: the presets and the read rows, shared by the connection
// wizard's step 4 and the consent screen.
//
// IT OWNS NO STATE. `selected` is the whole tick list of the surface hosting
// it, write rows included, and every change goes out through `onChange` as the
// complete next list. The host keeps the cache-clear box and the site-tools box
// (they are not reads and live in their own bordered boxes) and ticks them into
// the same list, which is why the preset claim below is derived over all of it.
//
// A PRESET SETS THE CHECKBOXES AND NOTHING ELSE (ruling 33). Pressing one
// replaces the whole list, so a write row ticked by hand is cleared by it. The
// claim on screen is derived from the list by `presetFor` on every render, so
// there is no code path that leaves a preset marked over a set that no longer
// matches it.
//
// WHICH ROWS CAN BE TICKED IS DECIDED BY THE HOST, through `offered`, and only
// ever from the reads this build can confer. A read the host did not offer is
// shown unticked and disabled; `mcp.content.read` is shown disabled with its
// reason whatever `offered` says, because no scope confers it.

/** Why the one seated-but-unreachable read cannot be ticked. */
const CONTENT_UNAVAILABLE_REASON =
  "Not available yet -- there are no content tools for a connection to call, so there is " +
  "nothing this permission could reach.";

/** Why a read this build can confer is not tickable on this surface. */
const NOT_OFFERED_REASON = "Not requested by this app";

export interface ReadCapabilityPickerProps {
  /** Every capability currently ticked on the host surface, reads and writes. */
  readonly selected: readonly string[];
  /** The complete next tick list. */
  readonly onChange: (next: readonly string[]) => void;
  /** The reads the host can confer. Anything outside the conferrable reads is ignored. */
  readonly offered: readonly string[];
  /** True while the host is busy, for example with a request in flight. */
  readonly disabled?: boolean;
}

export function ReadCapabilityPicker({
  selected,
  onChange,
  offered,
  disabled = false,
}: ReadCapabilityPickerProps) {
  const presets = capabilityPresets(offered);
  const activePreset = presetFor(selected, presets);
  const offeredReads = new Set(offered);

  return (
    <div className="space-y-3" data-testid="read-capability-picker">
      {/* THE PRESETS. A shortcut, not a mode: pressing one sets the checkboxes
          and nothing else, and the moment the set diverges the control says
          Custom. That is not enforced by a handler -- it is derived by
          presetFor, so no code path exists that could leave a preset selected
          over a set that no longer matches it. */}
      <div className="space-y-2">
        <p className="text-xs font-medium text-[var(--color-foreground)]">Start from</p>
        <div className="flex flex-wrap items-center gap-2" data-testid="capability-presets">
          {presets.map((preset) => {
            const active = activePreset === preset.id;
            return (
              <button
                key={preset.id}
                type="button"
                data-testid={`preset-${preset.id}`}
                aria-pressed={active}
                disabled={disabled}
                onClick={() => {
                  onChange([...preset.capabilities]);
                }}
                className={cn(
                  "inline-flex items-center gap-1.5 rounded-md border px-3 py-1.5 text-left text-xs transition-colors",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]",
                  disabled && "cursor-not-allowed opacity-70",
                  active
                    ? "border-[var(--color-primary)] bg-[var(--color-accent)] font-medium text-[var(--color-foreground)]"
                    : "border-[var(--color-border)] text-[var(--color-muted-foreground)] hover:bg-[var(--color-accent)]",
                )}
              >
                {/* THE TICK IS WHAT ACTUALLY SAYS "CHOSEN", and it is here
                    because border-and-fill alone did not. Pressing a preset
                    leaves focus on that button, and the focus ring is close
                    enough to the selected border that a just-diverged preset
                    went on LOOKING selected next to a freshly appeared Custom
                    -- aria-pressed said false and the picture said otherwise.
                    Found by opening the screenshot; the e2e assertion on
                    aria-pressed passed throughout. A glyph that only the
                    active branch renders cannot be imitated by a focus style.
                    ClientCard already marks selection this way. */}
                {active ? (
                  <Check aria-hidden="true" className="size-3.5 text-[var(--color-primary)]" />
                ) : null}
                {preset.label}
              </button>
            );
          })}
          {/* THE THIRD STATE, AND IT IS A STATEMENT RATHER THAN A BUTTON.
              Ruling 33 calls it unlabelled and there is nothing to press:
              Custom is where you ARE, not somewhere you go. Rendering it as a
              third button would invite an operator to click it and wonder why
              nothing happened. */}
          {activePreset === null ? (
            <span
              data-testid="preset-custom"
              className="rounded-md border border-dashed border-[var(--color-border)] px-3 py-1.5 text-xs font-medium text-[var(--color-foreground)]"
            >
              Custom
            </span>
          ) : null}
        </div>
        <p className="text-xs text-[var(--color-muted-foreground)]">
          {activePreset === null
            ? customNote(selected, presets)
            : (presets.find((p) => p.id === activePreset)?.description ?? "")}
        </p>
      </div>
      <ul className="space-y-2">
        {/* READS ONLY. The write row (mcp.cache.purge) never renders in this
            list -- it gets its own bordered box in the host, visibly distinct
            and never pre-ticked (design v7 S2.1). Filtering by CAPABILITY_KIND
            here, rather than by a written-out name, means a future read added
            to the vocabulary joins this list by construction and a future write
            does not. */}
        {KNOWN_CAPABILITIES.filter(
          (cap) => capabilityKind(cap) === "read" && !isAbilityCapability(cap),
        ).map((cap) => {
          const conferrable = (CONFERRABLE_READS as readonly string[]).includes(cap);
          const tickable = conferrable && offeredReads.has(cap);
          const checked = tickable && selected.includes(cap);
          return (
            <li key={cap}>
              <label
                className={cn(
                  "flex items-start gap-2 rounded-md border border-[var(--color-border)] p-2 text-sm",
                  !tickable && "cursor-not-allowed opacity-70",
                )}
              >
                <Checkbox
                  className="mt-0.5"
                  checked={checked}
                  disabled={!tickable || disabled}
                  onChange={(e) => {
                    onChange(withCapability(selected, cap, e.target.checked));
                  }}
                />
                <span>
                  <span className="block font-medium text-[var(--color-foreground)]">
                    {capabilityLabel(cap)}
                  </span>
                  <span className="block text-xs text-[var(--color-muted-foreground)]">
                    {CAPABILITY_DESCRIPTIONS[cap]}
                  </span>
                  {!conferrable ? (
                    <span className="block text-xs text-[var(--color-muted-foreground)]">
                      {CONTENT_UNAVAILABLE_REASON}
                    </span>
                  ) : !tickable ? (
                    <span
                      data-testid={`read-not-offered-${cap}`}
                      className="block text-xs text-[var(--color-muted-foreground)]"
                    >
                      {NOT_OFFERED_REASON}
                    </span>
                  ) : null}
                </span>
              </label>
            </li>
          );
        })}
      </ul>
      <p className="text-xs text-[var(--color-muted-foreground)]">
        Every row above this line is read-only. None of them can change WordPress content or
        configuration, whichever ones you pick.
      </p>
    </div>
  );
}
