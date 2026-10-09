import { CONFERRABLE_READS, type AbilityTicks, type Capability } from "./capabilities";

// The read picker's presets, shared by the connection wizard's step 4 and the
// consent screen so the two surfaces cannot offer different shortcuts or open
// on different defaults.
//
// A PRESET IS A SET OF READS AND NOTHING ELSE. `capabilityPresets` takes the
// reads a surface can offer and returns presets built only from those, so a
// surface that offers fewer reads gets presets that mean less, never presets
// that name a capability it cannot confer.
//
// THERE ARE TWO PRESETS BECAUSE THE OWNER RULED TWO. The deck draws four; the
// other two name a propose model the backend does not have (ruling 17), so
// building them would put a shortcut on screen that sets a capability no grant
// can hold.
//
// BOTH ARE DERIVED FROM THE VOCABULARY, NOT WRITTEN OUT. "Read everything" is
// every offered conferrable READ, so a capability added to CONFERRABLE_READS
// joins it by construction rather than by somebody remembering. Writing the
// names out here would be a second copy of the list that capabilities.ts
// exists to prevent, and its failure mode is a preset called "read everything"
// that quietly stops meaning it.
//
// NEITHER PRESET MAY INCLUDE THE WRITE. `mcp.cache.purge` needs its own
// per-call approval (ADR-061 option B) and is never pre-ticked on any surface
// and never part of a preset (design v7 S2.1, ruling 33). `capabilityPresets`
// filters its input through CONFERRABLE_READS, the reads-only list, so a caller
// that hands it a write name or `mcp.content.read` still gets a preset without
// it.
//
// THE TWO SITE-TOOLS ROWS ARE IN NO PRESET EITHER, and their opening state
// differs by surface: the connection wizard opens them clear, and the consent
// screen opens them ticked when the requesting app asked for mcp:site and the
// server offers them. A preset sets the whole tick list, so pressing one on the
// consent screen clears them again.

export type CapabilityPresetId = "basics" | "read-everything";

export interface CapabilityPreset {
  readonly id: CapabilityPresetId;
  readonly label: string;
  readonly description: string;
  /** The reads this preset ticks. Never a write, never mcp.content.read. */
  readonly capabilities: readonly string[];
}

/**
 * The preset both surfaces open on. It is the default the wizard has always
 * opened on, and dto.go's own default for an omitted capability list, so an
 * operator who changes nothing gets exactly what not answering would have
 * given them.
 */
export const DEFAULT_PRESET_ID: CapabilityPresetId = "basics";

const BASICS_READS: readonly Capability[] = ["mcp.sites.read"];

/**
 * The members of `names` this build can confer as a read, once each and in
 * vocabulary order. This is the one filter between "what a surface was told it
 * may offer" and "what a preset or a picker row may ever name".
 */
export function conferrableReadsIn(names: readonly string[]): readonly string[] {
  const wanted = new Set(names);
  return CONFERRABLE_READS.filter((cap) => wanted.has(cap));
}

function setKey(names: readonly string[]): string {
  return [...names].sort().join("|");
}

/**
 * The presets for a surface that can offer `offered` reads.
 *
 * A PRESET THAT WOULD TICK NOTHING IS NOT OFFERED, and neither is one that
 * repeats an earlier preset's set. An empty preset would match the empty
 * selection in `presetFor`, so a screen with nothing ticked would claim to be
 * on a named shortcut. A repeat could never be the active one, because the
 * first match wins. Both only arise when a surface offers fewer reads than the
 * full list; the wizard offers all of them and gets both presets.
 */
export function capabilityPresets(offered: readonly string[]): readonly CapabilityPreset[] {
  const reads = conferrableReadsIn(offered);
  const candidates: readonly CapabilityPreset[] = [
    {
      id: "basics",
      label: "Just the basics",
      capabilities: reads.filter((cap) => (BASICS_READS as readonly string[]).includes(cap)),
      description: "See which sites are in scope, and nothing else.",
    },
    {
      id: "read-everything",
      label: "Read everything",
      capabilities: reads,
      description: "Every read this connection could be given. It still cannot change anything.",
    },
  ];
  const seen = new Set<string>();
  return candidates.filter((preset) => {
    if (preset.capabilities.length === 0) return false;
    const key = setKey(preset.capabilities);
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

/**
 * Which preset the CURRENT selection is, or null for a custom set.
 *
 * DERIVED ON EVERY RENDER, NEVER STORED, AND THAT IS THE WHOLE DESIGN. Ruling
 * 33 says touching a checkbox moves the control to an unlabelled Custom state.
 * Storing "the operator pressed Read everything" and clearing it on each
 * checkbox change would implement that with a second piece of state that can
 * disagree with the set underneath -- and a label claiming a preset over a set
 * that has diverged is this component's signature defect, the same shape as the
 * rail claiming a step done while its action was blocked.
 *
 * Deriving it means the disagreement is not merely tested against, it is
 * UNCONSTRUCTIBLE: there is no second value for a label to read. Unticking a
 * row from "read everything" makes this return null on the very same render,
 * and re-ticking it makes the preset name come back on its own -- which is
 * correct, because the set genuinely is that preset again.
 *
 * `selected` is the WHOLE tick list, write rows included, so ticking the
 * cache-clear box moves a read-only preset to Custom: the preset's description
 * says "and nothing else", and it would be untrue over a set that holds a
 * write.
 */
export function presetFor(
  selected: readonly string[],
  presets: readonly CapabilityPreset[],
): CapabilityPresetId | null {
  const chosen = setKey(selected);
  const match = presets.find((preset) => setKey(preset.capabilities) === chosen);
  return match?.id ?? null;
}

/**
 * What a surface opens with: the default preset's reads, limited to what the
 * surface offers. Empty when the surface offers none of them, which leaves the
 * operator to tick what they want rather than defaulting to a read that is not
 * on offer.
 */
export function defaultCapabilities(offered: readonly string[]): readonly string[] {
  const preset = capabilityPresets(offered).find((p) => p.id === DEFAULT_PRESET_ID);
  return preset === undefined ? [] : preset.capabilities;
}

/** `selected` with `cap` ticked or unticked. Never adds a name twice. */
export function withCapability(
  selected: readonly string[],
  cap: string,
  ticked: boolean,
): readonly string[] {
  if (ticked) return selected.includes(cap) ? selected : [...selected, cap];
  return selected.filter((c) => c !== cap);
}

/**
 * `selected` with both site-tools rows set to `ticks`, in one step. The site-tools
 * box works out the next state of both rows (see nextAbilityTicks) and its host
 * applies it here, so a change that moves both rows is a single update rather
 * than two that could overwrite each other.
 */
export function withAbilityTicks(
  selected: readonly string[],
  ticks: AbilityTicks,
): readonly string[] {
  return withCapability(
    withCapability(selected, "mcp.ability.read", ticks.read),
    "mcp.ability.request",
    ticks.request,
  );
}
