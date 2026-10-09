import { withoutOrphanedRequest } from "./capabilities";

/** The capability payload a mint would send, or the reason there is none. */
export type MintCapabilitiesRequest =
  | { readonly ok: true; readonly capabilities: readonly string[] }
  | { readonly ok: false; readonly refusal: string };

/**
 * The capability payload for the CURRENT selection, or the reason there is
 * none -- the same shape and the same reason `mintScopeRequest` in the wizard
 * returns one, so the gate (`mintBlockedReason`) and the wire payload
 * (`TokenMintPanel`'s mint call) read one value rather than two derivations
 * of the same checkbox state.
 *
 * THE SAME PAYLOAD RULE AS THE CONSENT SCREEN. "Ask for changes" needs "see what
 * the site can do": a connection holding the request alone cannot call the tool
 * that carries one. `withoutOrphanedRequest` is the one rule for that, used here
 * and by the consent approval. The site-tools box already keeps the pair
 * together, so the wizard's own ticks cannot reach the bad state; the rule is
 * held again here so the payload is right whatever the tick list holds, and a
 * request dropped by it counts as not selected.
 *
 * THE ONLY REFUSAL: NOTHING IS CHECKED. dto.go's mintConnectionRequestDTO
 * treats an OMITTED `capabilities` field as the default preset
 * `["mcp.sites.read"]`, but an explicitly empty array is a different wire
 * value entirely -- it mints a connection that authenticates and can reach no
 * tool at all, because Authenticate refuses by name on every request. A
 * request naming no capabilities and a request naming none-on-purpose are not
 * the same thing, so this is refused client-side rather than silently
 * becoming the default or being sent as `[]`.
 */
export function mintCapabilitiesRequest(selected: readonly string[]): MintCapabilitiesRequest {
  const capabilities = withoutOrphanedRequest(selected);
  if (capabilities.length === 0) {
    return {
      ok: false,
      refusal:
        "No capability is selected, so this token would authenticate and be able to reach nothing. Pick at least one capability above, or leave Sites checked. An empty selection is refused rather than becoming the default.",
    };
  }
  return { ok: true, capabilities };
}
