import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import type { UseMutationResult } from "@tanstack/react-query";
import { useBlocker } from "@tanstack/react-router";
import { AlertTriangle, Check, ExternalLink, Lock } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { CopyableMono } from "@/components/shared/copyable-mono";
import { cn } from "@/lib/utils";
import {
  CAPABILITY_DESCRIPTIONS,
  CONFERRABLE_CAPABILITIES,
  KNOWN_CAPABILITIES,
  capabilityLabel,
} from "./capabilities";

import {
  CLIENT_TABLE_VERIFIED_AT,
  MCP_CLIENTS,
  PROTOCOL_FLOOR_VERSION,
  PROTOCOL_TARGET_VERSION,
  availableAuthMethods,
  CONFIG_PATH_GAP,
  SELF_HOSTED_PROXY_REQUIREMENT,
  type AuthAvailability,
  type AuthMethod,
  type McpClientRow,
} from "./client-table";
import { ConnectionContract } from "./connection-contract";
import { buildSnippet, type Snippet } from "./snippet";
import { SiteScopeStep, type SiteScopeSelection } from "./site-scope-step";
import {
  ConnectionsRequestError,
  useMintConnection,
  type MintConnectionInput,
  type MintedConnection,
} from "./use-ai-connections";
import { formatAbsolute } from "@/features/updates/schedule";
import {
  describeSiteScope,
  resolveSiteScope,
  resolveTagIds,
  type FleetSnapshot,
  type ResolvedSiteScope,
} from "@/features/mcp-consent/site-scope";

// The connection wizard (design §18). The specified spine, walked in order.
//
// THE ORDER IS 1, 2, 3, 4, 5, 6 AND IT IS THE SPEC'S OWN. The operator starts
// on the can/cannot contract, names the client, fixes the sites, fixes the
// capabilities, chooses how it signs in, and collects the setup artefact.
// Nothing here is deliberately non-monotonic: an earlier version of this file
// walked 2, 5, 3, 4, 6 and wrote that ordering up as a design decision. It was
// not one. It was the order the sections happened to be built in, and the
// operator reported it as the defect it is ("after step 2 it goes to step 5").
//
// THE CLIENT STILL COMES BEFORE THE AUTH METHOD, which is the one ordering
// constraint the design actually states: "the user picks the client and the
// system computes which authentication methods are possible, disabling the
// rest with the specific reason written into the disabled card -- never a
// generic 'unavailable'." Step 2 before step 5 satisfies that.
//
// WHAT THIS RENDERS AND WHAT IT DOES NOT. Specified steps 1 through 6 live
// here. Steps 7 to 10 are not built; the rail draws them as not yet available
// and the last built step offers no Continue rather than one leading nowhere.
// STEP 3 IS A REAL DECISION SURFACE: the wireframe puts "which sites" before
// "what it may do" on purpose, and mcp_grants already carries site_scope_mode,
// scope_tag_ids and scope_site_ids, so nothing about the selection is blocked
// on the backend.
//
// WHAT STEP 3 STILL CANNOT DO, SAID ON SCREEN RATHER THAN HIDDEN. The grant is
// created by the approval screen at /connect/ai, which the CLIENT redirects
// into; this wizard never calls it and has no channel to hand it an answer.
// So the choice made here is the operator arriving with the answer, not the
// answer being written. The same is already true of the connection name two
// sections down, and it is stated the same way rather than implied.
//
// THE CAPABILITIES STEP IS ASKED ON BOTH PATHS, AND THAT IS A CONSEQUENCE OF
// THE ORDER. The vocabulary is seated (policy.go: eight capability strings,
// seven of them conferrable via the read scope), and TokenMintPanel calls
// useMintConnection(), which POSTs to CONNECTIONS_PATH and accepts a
// `capabilities` list on the request (dto.go:264) and returns one on the
// response (dto.go:305). MintConnectionInput (use-ai-connections.ts) carries
// that field, and Section n=4 below ("Choose what it may do") is the picker
// that fills it.
//
// IT USED TO BE RENDERED ONLY FOR `method === "token"`, AND IN THE SPECIFIED
// ORDER THAT IS NOT EXPRESSIBLE. Step 4 comes BEFORE step 5, so at the moment
// this step is asked no auth method has been chosen: keying the step's
// existence on that answer would key it on an answer the operator has not
// given yet. The old arrangement only looked coherent because the built order
// put the auth step second.
//
// WHERE THE ANSWER ACTUALLY GOES, STATED ON THE STEP RATHER THAN IMPLIED. On
// the token path it is on the wire. On the browser sign-in path it is not: the
// client redirects into the approval screen at /connect/ai, which calls
// Approve (service.go:607); ApprovalRequest carries Principal, Consent,
// GrantName and SiteScope and nothing else, so that path has no channel for a
// capability answer today (tracked in GH #660). That is exactly the position
// site scope is already in one step earlier, and it is handled the same way:
// the step is asked, and it says what happens to the answer on each path. A
// step that vanished for half the operators would be a rail that changes
// length under them, which ruling 15 forbids.
//
// NO SNIPPET IS WRITTEN IN THIS FILE. Every block comes from buildSnippet, and
// snippet.test.ts fails the build if a config literal appears here.

type Step = 1 | 2 | 3 | 4 | 5 | 6;

/**
 * The rail this page renders is the design's own numbering (S29, "ADD MCP
 * CONNECTION: THE TEN STEPS"), not the six sections built so far. Showing only
 * the six built ones would show an operator six-tenths of a path and let them
 * believe that was the whole of it; the approved wizard has ten steps and the
 * operator is entitled to see all ten before starting.
 *
 * THE WALK IS MONOTONIC AND THE LOCAL NUMBER IS THE SPECIFIED NUMBER. There is
 * no map to keep in step any more, because there are no two numbering systems:
 * section n renders specified step n and the rail's nth segment is that same
 * step. The previous version of this file walked 2, 5, 3, 4, 6 and wrote the
 * discrepancy up as a design decision; WIZARD-SPEC lists the ten steps in
 * order and prescribes no reordering. The operator reported the symptom
 * exactly: after step 2 it went to step 5.
 *
 * `BUILT_ORDER` stays as the one written statement of the walk, so a test can
 * assert the SEQUENCE rather than individual steps and a future reorder cannot
 * pass silently by moving one entry.
 */
const BUILT_ORDER: readonly [Step, number][] = [
  [1, 1],
  [2, 2],
  [3, 3],
  [4, 4],
  [5, 5],
  [6, 6],
];

/**
 * The specified step numbers this wizard walks, in walk order. Exported for
 * the sequence assertion: a test that checked "step 3 exists" would still pass
 * against a reordered wizard, and that is the defect being fixed here.
 */
export const WALK_SPEC_ORDER: readonly number[] = BUILT_ORDER.map(([, spec]) => spec);

/** Each built section's local step, named rather than written as a bare number. */
const CONTRACT_LOCAL_STEP: Step = 1;
const CLIENT_LOCAL_STEP: Step = 2;
const SITE_SCOPE_LOCAL_STEP: Step = 3;
const CAPABILITY_LOCAL_STEP: Step = 4;
const AUTH_LOCAL_STEP: Step = 5;
const SETUP_LOCAL_STEP: Step = 6;


/**
 * What a rail segment IS, for the operator standing in front of it.
 *
 *   "built"     -- there is a section for it and the walk visits it.
 *   "not-built" -- no section exists yet. Specified steps 7 to 10 today.
 *
 * THERE IS NO "NOT ASKED ON THIS PATH" STATE ANY MORE. It existed for one
 * case, the capability picker on the browser sign-in path, and in the
 * specified order that case cannot arise: step 4 is asked before step 5, so no
 * auth answer exists to skip it on. Every operator walks the same six steps.
 */
type SpecStepAvailability = "built" | "not-built";

interface SpecStepDef {
  /** The specified step number (design S29), 1 through 10. */
  readonly n: number;
  /**
   * THE RAIL LABEL, AND IT IS THE SHORT ONE. Ruling 15 on the deck: "The
   * stepper's short labels (Start, Client, Sites, Capabilities, Auth, Setup,
   * Authorize, Confirm, Test, Done) are canonical for the rail; the long frame
   * titles are canonical for the screen heading." We shipped the long titles
   * in the rail, slash-separated, which is the frame heading standing in a
   * place it does not belong and reads as a paragraph rather than a stepper.
   */
  readonly rail: string;
  /**
   * The long frame title, canonical for the screen heading. Held here so the
   * rail and the section heading are two renderings of ONE row, and cannot
   * drift into naming the same step differently -- which is exactly the defect
   * reported: the rail called a section step 5 while its own heading said 2.
   */
  readonly heading: string;
  /** True when this specified step has a built section. */
  readonly built: boolean;
}

/** Which availability state one specified step is in. */
function specStepAvailability(s: SpecStepDef): SpecStepAvailability {
  return s.built ? "built" : "not-built";
}

/**
 * The steps this wizard walks, in order.
 *
 * ONE WALK, THE SAME FOR EVERY OPERATOR. Navigation reads this and nothing
 * else. It used to be filtered by the auth method, which was the mechanism the
 * non-monotonic order needed and which the specified order makes both
 * unnecessary and unusable -- the method is answered at step 5, after every
 * step the filter would have removed.
 */
function walkFor(): readonly [Step, number][] {
  return BUILT_ORDER.filter(([, spec]) => {
    const def = SPEC_STEPS.find((s) => s.n === spec);
    return def === undefined || specStepAvailability(def) === "built";
  });
}

/**
 * Where the operator actually stands: the position they asked for, clamped to
 * the first step in the walk whose gate refuses.
 *
 * THE CLAMP IS WHAT MAKES THE RAIL'S INVARIANT STRUCTURAL. The first blocked
 * step is a wall: an operator may stand on it and never past it, so every
 * position before the cursor is settled by construction, and "never shows a
 * step complete while the action it gates is blocked" needs no second
 * condition anywhere else to hold.
 *
 * IT IS EXPORTED, AND TESTED DIRECTLY, BECAUSE ITS SHARPEST CASE CANNOT BE
 * REACHED THROUGH THE DOM. Requesting a position PAST the wall needs an answer
 * to go bad behind the operator -- the fleet query refetching into a failure
 * while they stand on the setup step, which is what a window focus or a
 * reconnect does in production. Nothing in a mounted test can make the route
 * re-read a mocked hook without the operator navigating, and navigating is
 * exactly what resets the request. Removing the clamp therefore leaves every
 * rendered test green, which would make it look like dead code and invite its
 * deletion. The unit tests beside this function are what actually hold it.
 *
 * A requested step that is not in the walk at all (-1) means the path changed
 * under it: the operator was on the capability picker and went back to choose
 * browser sign-in, which does not ask that step. Falling back to the wall
 * rather than to the start keeps every answer they have already given.
 */
export function resolveCursorPos(
  walk: readonly [Step, number][],
  gates: readonly StepGate[],
  requestedStep: Step,
): number {
  const firstBlocked = gates.findIndex((g) => g.refusal !== null);
  const maxPos = firstBlocked === -1 ? walk.length - 1 : firstBlocked;
  const requestedPos = walk.findIndex(([local]) => local === requestedStep);
  return Math.min(requestedPos === -1 ? maxPos : requestedPos, maxPos);
}

// All ten, in specified order, so the operator sees the whole path before
// starting. Only six are `built: true`; the rest render as not-yet-available
// rather than being omitted or, worse, made to look done.
const SPEC_STEPS: readonly SpecStepDef[] = [
  { n: 1, rail: "Start", heading: "Start a connection", built: true },
  { n: 2, rail: "Client", heading: "Name it, pick the AI client", built: true },
  { n: 3, rail: "Sites", heading: "Choose which sites", built: true },
  { n: 4, rail: "Capabilities", heading: "Choose what it may do", built: true },
  { n: 5, rail: "Auth", heading: "Choose how it authenticates", built: true },
  { n: 6, rail: "Setup", heading: "Get the setup artefact", built: true },
  { n: 7, rail: "Authorize", heading: "Connect and authorize", built: false },
  { n: 8, rail: "Confirm", heading: "WPMgr confirms connection is live", built: false },
  // "Test" in the rail, "Verify with a first read" as the heading -- ruling 15
  // names this pair explicitly, because the deck uses both words for step 9.
  { n: 9, rail: "Test", heading: "Verify with a first read", built: false },
  { n: 10, rail: "Done", heading: "Done: tool list and first prompt", built: false },
];

/**
 * Whether the site-scope step is actually done, for every reason
 * `mintBlockedReason` below can refuse to mint on it -- READ BY BOTH THE
 * BUTTON AND THE RAIL FROM ONE FUNCTION, `siteScopeReadiness`, rather than
 * each asking a narrower question of its own.
 *
 * THIS TYPE EXISTS BECAUSE A NARROWER ONE WAS FOUND WRONG THREE TIMES, THROUGH
 * THREE DIFFERENT DOORS, IN ONE REVIEW PASS. The rail first asked only
 * `scope.kind`, which answers "did the fleet read resolve" and says nothing
 * about `mintScopeRequest`'s OWN refusals: `tags-unresolved` (the tag
 * registry, a different read from the fleet) and `unselected` (the operator
 * has not picked anything under 'tags' or 'list' mode yet). Both left minting
 * blocked while the rail called step 3 done and put `aria-current` on step 6.
 * Patching the rail's own condition a second time would have been a fourth
 * door on the same room; this widens the ONE predicate both consumers read
 * instead.
 */
type SiteScopeReadiness = "loading" | "failed" | "tags-unresolved" | "unselected" | "resolved";


/**
 * Whether the capability picker is actually done, for the one reason mint can
 * refuse it on the token path: nobody has been left checked. Deliberately a
 * NARROWER union than SiteScopeReadiness -- there is no fleet read behind this
 * step, so there is no "loading" or "failed" state for it to be in, only
 * "an operator has not settled on an answer" or "they have."
 *
 * `"unselected"` is a member of SiteScopeReadiness too, so the rail's
 * per-segment rendering below (which already prints "(not chosen yet)" for
 * that string) needs no new branch to cover this step as well.
 */
type CapabilityReadiness = "unselected" | "resolved";


export interface ConnectWizardProps {
  /** Absolute MCP endpoint for this deployment. Passed in, never assembled here. */
  endpointUrl: string;
  /**
   * What the route loaded of the fleet, or null when the load has not finished
   * or failed. NULL IS NOT AN EMPTY SNAPSHOT, and a full page is NOT a whole
   * fleet. Both facts travel inside FleetSnapshot rather than being re-derived
   * on this screen.
   */
  fleet: FleetSnapshot | null;
  /** The tag registry, or null when we could not read it. Null is not empty. */
  tags: readonly { readonly id: string; readonly name: string }[] | null;
  tagsBySiteId: Readonly<Record<string, readonly string[]>>;
  sitesLoading: boolean;
  /** Where the approval flow lives, for the closing instructions. */
  className?: string;
}

export function ConnectWizard({
  endpointUrl,
  fleet,
  tags,
  tagsBySiteId,
  sitesLoading,
  className,
}: ConnectWizardProps) {
  const [clientId, setClientId] = useState<string | null>(null);
  const [method, setMethod] = useState<AuthMethod | null>(null);
  const [name, setName] = useState("Fleet manager");
  // THE MINT AND ITS REVEAL LIVE HERE, ABOVE EVERY CONTROL THAT CAN UNMOUNT
  // THE PANEL. They used to live inside TokenMintPanel, which step 4 renders
  // only for method 'token' on a client that offers it; changing the method,
  // or picking a client that cannot use a token, unmounted that panel WHILE
  // ITS REQUEST WAS STILL OPEN. The server had already created the credential,
  // and the response then wrote the one-time plaintext through a setState on a
  // component nobody was looking at. The outcome is the worst this screen can
  // produce: a live organisation credential that authenticates, whose plaintext
  // was shown to no one, that nobody knows to revoke because nobody knows it
  // exists. Holding the state at the wizard means a response always lands on a
  // mounted surface, whatever the operator clicked while it was in the air.
  const mint = useMintConnection();
  const [reveal, setReveal] = useState<MintedReveal | null>(null);
  // NO DEFAULT 'all'. The consent screen refuses one for the same reason
  // (m124 DECISION 1) and so does the schema: a scope nobody chose must not
  // begin life as the widest one. 'list' holding nothing is the wireframe's
  // opening state, and it is empty on purpose.
  const [selection, setSelection] = useState<SiteScopeSelection>({
    mode: "list",
    tagNames: [],
    siteIds: [],
  });
  // DEFAULTS TO `["mcp.sites.read"]`, NOT `[]` AND NOT ALL SEVEN. Empty would
  // silently mint a token that can reach nothing the moment an operator
  // skipped this step without reading it -- the same defect this project's
  // signature bug takes elsewhere, one layer up: a state nobody chose read as
  // a deliberate answer. All seven would defeat the point of asking at all,
  // the same reasoning the site-scope step's "no default 'all'" comment above
  // already gives. Sites-read matches dto.go's own default for an OMITTED
  // field, so an operator who changes nothing here ends up with exactly what
  // they would have gotten by not answering.
  const [capabilities, setCapabilities] = useState<readonly string[]>(["mcp.sites.read"]);

  const client = useMemo(
    () => MCP_CLIENTS.find((c) => c.id === clientId) ?? null,
    [clientId],
  );

  const methods = client === null ? [] : availableAuthMethods(client);

  // The current step is derived, never stored, so it cannot disagree with the
  // answers. Picking a different client with an incompatible method drops the
  // method rather than carrying a stale one into step 3.
  //
  // WHERE THE OPERATOR ASKED TO BE. Stored, because the wizard shows one step
  // at a time and Continue/Back are the only way through: a derived position
  // cannot express "I have answered this and moved on" or "I have gone back to
  // change it", which is the whole interaction. It is a REQUEST, not the
  // answer -- `resolveCursorPos` clamps it, so this state can never put the
  // operator past a step that is still blocked.
  const [requestedStep, setRequestedStep] = useState<Step>(1);

  // Resolved once here rather than inside the mint panel, so the SAME answer
  // gates the mint button and is described back in the one-time reveal -- a
  // panel that re-resolved its own copy could disagree with the gate that let
  // it run.
  const scope: ResolvedSiteScope = useMemo(
    () =>
      resolveSiteScope({
        mode: selection.mode,
        selectedTagNames: selection.tagNames,
        selectedSiteIds: selection.siteIds,
        fleet,
        tagsBySiteId,
        sitesLoading,
      }),
    [selection, fleet, tagsBySiteId, sitesLoading],
  );

  // NULL means a selected tag name no longer resolves to an id -- see
  // resolveTagIds. The mint panel refuses to submit on null rather than
  // sending the smaller array that survives, because that would narrow the
  // connection's reach without telling the operator.
  const scopeTagIds: readonly string[] | null = useMemo(
    () => (selection.mode === "tags" ? resolveTagIds(selection.tagNames, tags) : []),
    [selection.mode, selection.tagNames, tags],
  );

  // The exact site-scope fields a mint would send, or the reason it can send
  // none. Built ONCE, here, and both the gate and the request read this same
  // value -- the gate cannot approve a payload different from the one that
  // goes out, because there is only one payload.
  const scopeRequest = useMemo(
    () => mintScopeRequest(selection.mode, scopeTagIds, selection.siteIds),
    [selection.mode, scopeTagIds, selection.siteIds],
  );

  // THE RAIL'S SITE-SCOPE STATE, FROM `siteScopeReadiness` -- THE SAME
  // FUNCTION `mintBlockedReason` CALLS BELOW, NEVER A NARROWER RE-DERIVATION.
  // This found the same defect a second time: reading only `scope.kind` fixed
  // the case where the FLEET read hadn't resolved, and left the rail calling
  // step 3 done while `mintScopeRequest` was still refusing to mint on
  // `tags-unresolved` (the TAG registry, a different read) or on an empty
  // selection the operator had not made yet. One function, read by both the
  // button and the rail, is what makes a fourth door impossible rather than
  // merely unlikely.
  const siteScopeState: SiteScopeReadiness = siteScopeReadiness(scope, scopeRequest);
  void siteScopeState;

  // THE CAPABILITY PAYLOAD, OR THE REASON THERE IS NONE -- built once, here,
  // the same pattern as `scopeRequest` two blocks up and for the same reason:
  // the gate and the mint call must read one value, never two derivations of
  // the same selection that could disagree.
  const capabilitiesRequest = useMemo(
    () => mintCapabilitiesRequest(capabilities),
    [capabilities],
  );

  // THE RAIL'S CAPABILITY STATE, mirroring siteScopeState immediately above:
  // one function, read by both the rail and `mintBlockedReason` below, so
  // "step 4 reads done" and "mint will actually accept this" cannot drift
  // apart the way `scope.kind` alone once did for site scope.
  const capabilityState: CapabilityReadiness = capabilityReadiness(capabilitiesRequest);
  void capabilityState;

  // ONE CONTEXT, ONE PREDICATE, EVERY CONSUMER. The rail, the Continue button
  // and the mint button all read `stepGate` over this value and nothing else,
  // so "the rail says done", "Continue is offered" and "mint will be accepted"
  // cannot disagree -- they are the same call.
  const gateContext: StepGateContext = {
    clientChosen: client !== null,
    method,
    methodOffered: methods.length > 0,
    scope,
    scopeRequest,
    capabilitiesRequest,
  };

  // The steps this wizard walks, 1 through 6 in order, the same for everyone.
  const walk = walkFor();
  const gates: readonly StepGate[] = walk.map(([local]) => stepGate(local, gateContext));
  const cursorPos = resolveCursorPos(walk, gates, requestedStep);
  const currentLocal: Step = walk[cursorPos]![0];
  const currentGate: StepGate = gates[cursorPos]!;
  const isLastBuiltStep = cursorPos === walk.length - 1;

  // A mint is a network round trip, and every control above is live during it.
  // Disabling them is the FIRST half of the fix (the operator is not offered a
  // way to walk out on an open request); holding the state above them is the
  // second half, and it is the half that holds when this one is bypassed.
  const mintInFlight = mint.isPending;

  // THE ROUTE IS A CONTROL TOO. Locking the client and method cards closed the
  // doors inside this screen and left the front one open: the back link in the
  // page header, a sidebar entry, the browser's own back button all unmount the
  // wizard mid-request, and the response then lands on nothing. The server has
  // already created a working credential whose one-time plaintext is shown to
  // no one, that nobody knows to revoke because nobody knows it exists. So the
  // same reasoning that disables the cards is extended to the route.
  //
  // `disabled` is what makes this NOT a trap. When no mint is open the blocker
  // is never installed at all, so shouldBlockFn cannot run and cannot decide
  // wrongly; when the mint settles -- resolved OR rejected -- isPending goes
  // false, the effect tears the block down, and the operator leaves freely. A
  // block that outlived its request would strand them on a page they cannot
  // exit, which is a worse defect than the one being fixed.
  const [navigationHeld, setNavigationHeld] = useState(false);
  // WHAT THIS DOES NOT COVER, said here rather than left to be discovered:
  // closing the tab, reloading, or typing an address. Those never reach the
  // router. `enableBeforeUnload` is off on purpose rather than by omission --
  // the browser's prompt is generic, cannot name the credential, is suppressed
  // without prior interaction, and preserves nothing if the operator confirms.
  // It would make this screen look protected against a case it does not
  // protect, which this file refuses to do elsewhere for the same reason. Only
  // owning the mint response outside the render tree closes those, and that is
  // filed separately.
  useBlocker({
    disabled: !mintInFlight,
    enableBeforeUnload: false,
    shouldBlockFn: () => {
      // A refusal the operator cannot see reads as a broken link, and they
      // click it again harder. Recording it here is what puts a reason on the
      // screen naming the credential being created and shown once.
      setNavigationHeld(true);
      return true;
    },
  });
  // The reason is scoped to the request that caused it. Once nothing is in
  // flight there is nothing being held, so a notice still claiming otherwise
  // would be a false statement about the page the operator is now free to
  // leave. Adjusted during render, the same way the stale-error reset below
  // is, rather than in an effect that would paint the lie for one frame.
  if (navigationHeld && !mintInFlight) setNavigationHeld(false);

  // Clearing a STALE ERROR when the configuration changes, and NOTHING ELSE.
  // This used to clear the reveal too, which is the same defect as the unmount
  // race wearing different clothes: a single keystroke in the name field, after
  // a token had been revealed, destroyed the only copy of a live credential.
  // The reveal now carries the configuration it was minted for (MintedReveal),
  // so it cannot go stale and has no reason to be thrown away; it is dismissed
  // by an explicit act and by nothing else.
  const configKey = `${selection.mode}|${selection.siteIds.join(",")}|${(scopeTagIds ?? []).join(",")}|${name.trim()}`;
  const [lastConfigKey, setLastConfigKey] = useState(configKey);
  if (lastConfigKey !== configKey) {
    setLastConfigKey(configKey);
    if (mint.isError) mint.reset();
  }

  const snippet: Snippet | null = useMemo(() => {
    if (client === null || method === null) return null;
    try {
      return buildSnippet({
        client,
        endpointUrl,
        serverName: name,
        authMethod: method,
      });
    } catch {
      // buildSnippet throws when the table says this combination cannot work.
      // Null renders a refusal below; it never renders a best-effort block.
      return null;
    }
  }, [client, method, endpointUrl, name]);

  function chooseClient(next: McpClientRow) {
    setClientId(next.id);
    // Drop a method the new client cannot use rather than carrying it forward.
    setMethod((current) =>
      current !== null && availableAuthMethods(next).includes(current) ? current : null,
    );
  }

  return (
    <div className={cn("space-y-6", className)}>
      <StepRail walk={walk} cursorPos={cursorPos} currentGate={currentGate} />

      {/* THE ONE AND ONLY PLACE A REVEAL IS RENDERED, and it is deliberately
          outside every step. A second render site inside step 4 would restore
          exactly the fragility being removed: the token would be visible only
          while the conditions that mount step 4 happen to hold. Here it is
          governed by one thing, whether a token exists, so no client, method or
          step change can take it off the screen. */}
      {reveal !== null ? (
        <TokenReveal
          reveal={reveal}
          onDismiss={() => {
            setReveal(null);
            mint.reset();
          }}
        />
      ) : null}

      {navigationHeld ? (
        <p
          role="alert"
          data-testid="navigation-held"
          className="rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 p-3 text-sm text-[var(--color-foreground)]"
        >
          We kept you on this page. A connection token is being created right now, and it is shown
          once, so leaving before it arrives would leave a live credential on your organisation
          that nobody holds and nobody knows to revoke. This releases the moment the request
          finishes, whether it succeeds or fails.
        </p>
      ) : null}

      {mintInFlight ? (
        <p
          role="status"
          className="rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 p-3 text-sm text-[var(--color-foreground)]"
        >
          Minting a connection token. The client and sign-in choices are held until it finishes,
          because the token it produces is shown once and leaving now would strand a live
          credential nobody holds.
        </p>
      ) : null}

      {currentLocal === CONTRACT_LOCAL_STEP ? (
        <Section
          specN={1}
          hint="Read this before anything is created. It is the whole of what a connection is allowed to be."
        >
          <div className="space-y-3">
            {/* THE SAME COMPONENT THE /ai INDEX RENDERS, NOT A SECOND COPY OF
                THE CONTRACT. Two renderings of the eight specified strings
                would be two places they can drift, and the negative half is
                the product's entire safety argument -- the half that must not
                quietly soften on a later fidelity pass. ConnectionContract
                owns the strings and its own guard test owns what may never
                appear in them. */}
            <ConnectionContract />
            {/* WHY THIS SENTENCE IS ON THE FIRST STEP AND NOWHERE ELSE. An
                operator who has just pressed "New connection" does not know
                whether they have already made something. Nothing is written
                until step 6 mints, or until the client's own approval screen
                approves; saying so here is what makes Back and closing the tab
                obviously safe, rather than something to be careful about. */}
            <p
              data-testid="wizard-nothing-created-yet"
              className="text-xs text-[var(--color-muted-foreground)]"
            >
              Nothing is created yet. This is a wizard, not a draft row. No credential exists and
              no grant is written until you finish; leaving now leaves nothing behind.
            </p>
          </div>
        </Section>
      ) : null}

      {currentLocal === CLIENT_LOCAL_STEP ? (
      <Section
        specN={2}
        // The one narrowing, and its reason: this section picks the client and
        // does not yet ask for the name the deck s step 2 title promises.
        narrowerTitle="Pick your client"
        hint="Everything after this is computed from your answer, so nothing asks you twice."
      >
        <ul className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
          {MCP_CLIENTS.map((c) => (
            <li key={c.id}>
              <ClientCard
                client={c}
                selected={c.id === clientId}
                locked={mintInFlight}
                onSelect={() => chooseClient(c)}
              />
            </li>
          ))}
        </ul>
      </Section>
      ) : null}

      {currentLocal === AUTH_LOCAL_STEP && client !== null ? (
        <Section
          specN={5}
          hint={`Computed from ${client.name}, which you picked at step 2. A method it cannot use is disabled with the reason.`}
        >
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <AuthCard
              method="oauth"
              title="Sign in through your browser"
              // NO REFRESH IS PROMISED, BECAUSE NO REFRESH EXISTS. The design
              // frame says "the client stores a token it refreshes itself";
              // apps/api/internal/mcp/service.go:140 says the opposite in as
              // many words -- "There is no refresh_token grant: the connection
              // token's lifetime is the connection's, and nothing here mints a
              // refresh token" -- and discovery_test.go:256 drives the
              // validator with "refresh_token" to prove the discovery document
              // refuses it. A card promising a self-maintaining connection on
              // the screen where the operator picks their auth method is the
              // same defect this branch removed from the connections screen:
              // the deck is wrong here and the server is right. The expiry is
              // stated instead, because that is the thing that will actually
              // happen to them.
              body="You approve the connection on a WPMgr page and the client stores the token it is issued. Nothing secret is ever shown to you or pasted anywhere. That token does not refresh itself, so the connection stops working when it expires."
              recommendation={recommendationFor("oauth", client.name, methods)}
              availability={client.auth.oauth}
              selected={method === "oauth"}
              locked={mintInFlight}
              onSelect={() => setMethod("oauth")}
            />
            <AuthCard
              method="token"
              title="Use a connection token"
              body="We show a token once. You put it in your environment, and the client sends it as a header. This is the documented path for CI, containers and SSH, not a fallback for when sign-in fails."
              recommendation={recommendationFor("token", client.name, methods)}
              availability={client.auth.token}
              selected={method === "token"}
              locked={mintInFlight}
              onSelect={() => setMethod("token")}
            />
          </div>
          {methods.length === 0 ? (
            <p
              role="alert"
              className="mt-3 rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 p-3 text-sm text-[var(--color-foreground)]"
            >
              We cannot connect {client.name} yet. Neither method is confirmed to work with it, and
              we would rather say so than send you down a path we have never seen finish.
            </p>
          ) : null}

          {/* WHY THERE IS NO THIRD CARD. Without this, a reader who knows the
              OAuth device-code flow reads a disabled browser card on a headless
              client as an oversight, and the connection token as our fallback.
              It is neither. The date is rendered from the client table's own
              verified-at constant rather than written here, so this paragraph
              goes stale visibly instead of quietly. */}
          <div className="mt-3 rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 p-3">
            <p className="text-xs font-medium text-[var(--color-foreground)]">
              Why there is no “enter this code on another device” option
            </p>
            <p className="mt-1 text-xs text-[var(--color-muted-foreground)]">
              A device-code flow would solve this cleanly, and no MCP client implements one. We
              checked every client on this list on {CLIENT_TABLE_VERIFIED_AT}. A grant nothing can
              initiate is a grant nobody can use, so we have not built one. A scoped, revocable
              connection token does the same job today.
            </p>
          </div>
        </Section>
      ) : null}

      {/* NO `method !== null` GUARD ANY MORE, AND ITS ABSENCE IS THE POINT.
          Step 3 is asked before step 5, so requiring a method here would
          require an answer the operator has not been asked for and gate the
          section on a state it can never be in. */}
      {currentLocal === SITE_SCOPE_LOCAL_STEP && client !== null ? (
        <Section
          specN={3}
          hint="Chosen before capabilities, on purpose. What a connection may do is only meaningful once you have fixed what it may do it to."
        >
          <SiteScopeStep
            selection={selection}
            onSelectionChange={setSelection}
            fleet={fleet}
            tags={tags}
            tagsBySiteId={tagsBySiteId}
            sitesLoading={sitesLoading}
          />
          {/* WHERE THE ANSWER GOES, AND IT DEPENDS ON A CHOICE NOT YET MADE.
              This footnote used to state the browser sign-in outcome flatly,
              which was safe only while the auth step came first. At step 3 no
              method has been chosen, so it is written as the forward
              conditional it actually is. Stating it now rather than at step 5
              is deliberate: the operator is making the selection here, and the
              time to say what becomes of it is while they are making it. */}
          <p className="mt-3 text-xs text-[var(--color-muted-foreground)]">
            What happens to this answer depends on the sign-in method you choose at step 5. With a
            connection token it is sent with the token and is what the connection is scoped to.
            With browser sign-in the client opens the approval screen itself, so nothing carries
            this selection there and it asks for the scope again. Deciding it here is how you get
            there with the answer ready.
          </p>
        </Section>
      ) : null}

      {/* ASKED ON EVERY PATH, because at step 4 there is no path yet -- the
          method is chosen at step 5. The old `method === "token"` condition
          was only expressible while the auth step came second; see the
          file-top comment. Where the answer ends up is stated on the step
          itself, the same way step 3 states it one step earlier. */}
      {currentLocal === CAPABILITY_LOCAL_STEP && client !== null ? (
        <Section
          specN={4}
          hint="Every capability here is read-only. Nothing on this list can change WordPress content or configuration."
        >
          <div className="space-y-3">
            {/* STEP 3 IS BEHIND THE OPERATOR, so "already given" is a true
                statement about an answer they have made. It was not, in the
                order this wizard used to walk: step 4 came before step 3 was
                reached on screen, and this sentence claimed an answer that did
                not exist yet. */}
            <p className="text-xs text-[var(--color-muted-foreground)]">
              {describeSiteScope(scope)} This decides what it may see there -- how many sites
              it reaches is step 3's answer, already given.
            </p>
            <ul className="space-y-2">
              {KNOWN_CAPABILITIES.map((cap) => {
                const conferrable = (CONFERRABLE_CAPABILITIES as readonly string[]).includes(cap);
                const checked = capabilities.includes(cap);
                return (
                  <li key={cap}>
                    <label
                      className={cn(
                        "flex items-start gap-2 rounded-md border border-[var(--color-border)] p-2 text-sm",
                        !conferrable && "cursor-not-allowed opacity-70",
                      )}
                    >
                      <Checkbox
                        className="mt-0.5"
                        checked={checked}
                        disabled={!conferrable || mintInFlight}
                        onChange={(e) => {
                          const next = e.target.checked;
                          setCapabilities((current) =>
                            next
                              ? current.includes(cap)
                                ? current
                                : [...current, cap]
                              : current.filter((c) => c !== cap),
                          );
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
                            Not available yet -- there are no content tools for a connection to
                            call, so there is nothing this permission could reach.
                          </span>
                        ) : null}
                      </span>
                    </label>
                  </li>
                );
              })}
            </ul>
            <p className="text-xs text-[var(--color-muted-foreground)]">
              These are all read-only. No capability on this screen can change WordPress
              content or configuration, whichever ones you pick.
            </p>
            {/* The same forward conditional step 3's footnote carries, for the
                same reason and about the same step-5 answer. Two steps whose
                answers travel differently by path must both say so, or the one
                that stays silent reads as the one that always travels.

                THE BROWSER SIGN-IN HALF NAMES WHAT THE CONNECTION ENDS UP
                WITH, RATHER THAN CLAIMING THE QUESTION IS ASKED ELSEWHERE. It
                used to say permissions were "settled there instead", which was
                true only while POST /api/v1/oauth/mcp/consent had no capability
                field. It has one now (dto.go's approvalRequestDTO.Capabilities)
                and this app does not yet send it, so an OMITTED field is what
                reaches the server; service.go:871 resolves that through
                resolveGrantCapabilities to DefaultGrantCapabilities(), which
                policy.go:273 defines as exactly CapSitesRead. Naming that
                outcome is a fact the operator can check against the connection
                afterwards. "Permissions are chosen on the approval screen" was
                a promise about a control that screen does not have. */}
            <p className="text-xs text-[var(--color-muted-foreground)]">
              Where this answer goes depends on step 5. With a connection token it is sent with
              the mint request and is what the connection holds. With browser sign-in your client
              opens the approval screen itself, so this wizard has no way to hand it your answer:
              that connection is created able to read your sites and nothing else, whatever you
              pick here.
            </p>
            {/* NO PRIVATE REFUSAL PANEL HERE. `capabilitiesRequest.refusal` is
                the exact string `stepGate`'s CAPABILITY_LOCAL_STEP branch
                returns as `gate.refusal`, and WizardNav below already renders
                that once, next to Continue, for every step that can refuse.
                A second panel in this section rendered the identical sentence
                twice on screen; it predates the one-step-at-a-time conversion,
                from when this step had no Continue to attach a reason to. */}
          </div>
        </Section>
      ) : null}

      {currentLocal === SETUP_LOCAL_STEP && client !== null && method !== null ? (
        <Section
          specN={6}
          hint="Generated for this client and the sign-in method you chose at step 5. Every difference below is a real difference between clients."
        >
          <div className="space-y-4">
            <div className="max-w-sm space-y-1.5">
              <Label htmlFor="wizard-connection-name">Name this connection</Label>
              <Input
                id="wizard-connection-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Fleet manager"
              />
              <p className="text-xs text-[var(--color-muted-foreground)]">
                {/* THE OLD COPY HERE WAS FALSE. It promised this name appears on
                    the approval screen, and nothing carries it there: the client
                    starts the OAuth flow itself, so this page never hands the
                    name to /connect/ai, which asks for its own. Sending an
                    operator to look for something that will not be there is a
                    defect. Carrying it through would need a parameter the flow
                    does not have, so the copy is corrected rather than the
                    behaviour invented. */}
                Used as the server key in the config below. The approval screen asks you to name
                the connection separately, so this name stays on your machine.
              </p>
            </div>

            {snippet === null ? (
              <p
                role="alert"
                className="rounded-md border border-[var(--color-destructive)]/40 bg-[var(--color-destructive)]/5 p-3 text-sm"
              >
                We have no verified setup for {client.name} with this method, so there is nothing to
                copy. Copying a config we are guessing at would cost you an hour finding out it does
                not work.
              </p>
            ) : (
              <SnippetBlock client={client} snippet={snippet} />
            )}

            {method === "oauth" ? (
              <NextSteps clientName={client.name} />
            ) : (
              <TokenMintPanel
                mint={mint}
                revealed={reveal !== null}
                onMinted={setReveal}
                clientId={client.id}
                clientName={client.name}
                name={name}
                scope={scope}
                scopeRequest={scopeRequest}
                capabilitiesRequest={capabilitiesRequest}
              />
            )}
          </div>
        </Section>
      ) : null}

      <WizardNav
        gate={currentGate}
        isLastBuiltStep={isLastBuiltStep}
        canGoBack={cursorPos > 0}
        locked={mintInFlight}
        onBack={() => setRequestedStep(walk[cursorPos - 1]![0])}
        onContinue={() => setRequestedStep(walk[cursorPos + 1]![0])}
      />

      <p className="text-xs text-[var(--color-muted-foreground)]">
        We negotiate protocol {PROTOCOL_TARGET_VERSION} and accept nothing below{" "}
        {PROTOCOL_FLOOR_VERSION}.
      </p>
    </div>
  );
}

/**
 * EVERY STATE ONE RAIL SEGMENT CAN BE IN, AS ONE CLOSED UNION. A segment has
 * exactly one of these, and every visual it renders is looked up from it in
 * RAIL_SEGMENT_STYLES below.
 */
type RailSegmentState =
  | "not-built"
  | "current"
  | "completed"
  | "upcoming"
  | SiteScopeReadiness
  | CapabilityReadiness;

interface RailSegmentStyle {
  /** Circle styling on top of the base ring. */
  readonly circle: string;
  readonly label: string;
  /** The visible reason, if the state has one. */
  readonly suffix: string | null;
}

/**
 * THE ONE PLACE A RAIL SEGMENT'S APPEARANCE IS DECIDED.
 *
 * Every visual a segment renders is looked up here from its single state
 * value, so a style that contradicts the state is not expressible: there is no
 * second boolean for a style to read. The blocked states share the "upcoming"
 * circle deliberately -- no fill, no ring, nothing that reads as progress on a
 * step whose action is being refused.
 *
 * WHAT IS DELIBERATELY NOT IN THIS TABLE: whether a segment is the operator's
 * current position. That is not a property of a state value, because several
 * segments can hold a blocked state at once; it is a property of the rail, has
 * exactly one answer, and is decided once in StepRail.
 */
const RAIL_SEGMENT_STYLES: Record<RailSegmentState, RailSegmentStyle> = {
  "not-built": { circle: "opacity-70", label: "italic opacity-70", suffix: null },
  upcoming: { circle: "", label: "", suffix: null },
  completed: {
    circle:
      "border-[var(--color-primary)] bg-[var(--color-primary)] text-[var(--color-primary-foreground)]",
    label: "text-[var(--color-foreground)]",
    suffix: null,
  },
  current: {
    circle: "border-2 border-[var(--color-primary)] text-[var(--color-primary)]",
    label: "font-medium text-[var(--color-foreground)]",
    suffix: null,
  },
  // THE BLOCKED FOUR. Each keeps its own distinct reason on screen -- an
  // operator told nothing when a read has actually failed keeps waiting for a
  // state that is never coming -- and NONE of them gets the ring or the fill,
  // because the step they name cannot be completed from where the operator is.
  // UNREACHABLE, AND STILL EXHAUSTIVE. A segment only takes a readiness value
  // while that step is BLOCKING, and blocking is false exactly when the value
  // is "resolved" -- so this row is never looked up. It is written as the
  // no-claim style rather than omitted, because omitting it would mean typing
  // the table as a partial record, and a partial record is how a future state
  // gets added with no style and renders as whatever the base class happens to
  // be. If it is ever reached, it claims nothing.
  resolved: { circle: "", label: "", suffix: null },
  loading: { circle: "", label: "", suffix: " (loading)" },
  failed: {
    circle: "border-[var(--color-destructive)] text-[var(--color-destructive)]",
    label: "text-[var(--color-destructive)]",
    suffix: " (failed to load)",
  },
  "tags-unresolved": { circle: "", label: "", suffix: " (tags still loading)" },
  unselected: { circle: "", label: "", suffix: " (not chosen yet)" },
};

/**
 * Back and Continue, and the reason Continue is refused.
 *
 * WHAT BACK DOES TO ANSWERS ALREADY GIVEN: NOTHING. Every answer lives on the
 * wizard, above every section, so a step that is not on screen still holds
 * what was typed into it -- Back is a cursor move and nothing else. That is
 * what makes a confirmation dialog wrong here rather than merely annoying:
 * there is nothing to confirm. (Changing a CLIENT can still drop an
 * incompatible auth method, in `chooseClient`. That is a consequence of
 * changing an answer, which the operator did deliberately, not of navigating.)
 *
 * CONTINUE IS DISABLED WITH ITS REASON ON SCREEN, never silently. Same shape
 * as the mint button below, and the reason comes from the same `stepGate` call
 * the rail reads, so a refused Continue and a rail segment that is not marked
 * done are one fact stated twice, never two facts that can disagree.
 *
 * ON THE LAST BUILT STEP THERE IS NO CONTINUE AT ALL. Specified steps 7 to 10
 * are not built, so a Continue here would be a control leading nowhere. The
 * step's own action is the terminus instead -- the mint button on the token
 * path, the client instructions on the OAuth path -- and the rail already
 * shows the four remaining segments as not yet available.
 */
function WizardNav({
  gate,
  isLastBuiltStep,
  canGoBack,
  locked,
  onBack,
  onContinue,
}: {
  gate: StepGate;
  isLastBuiltStep: boolean;
  canGoBack: boolean;
  locked: boolean;
  onBack: () => void;
  onContinue: () => void;
}) {
  const blocked = gate.refusal !== null;
  return (
    <div className="space-y-2 border-t border-[var(--color-border)] pt-4">
      {!isLastBuiltStep && blocked ? (
        <p
          role="status"
          data-testid="continue-blocked"
          className="rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 p-3 text-sm text-[var(--color-foreground)]"
        >
          {gate.refusal}
        </p>
      ) : null}
      <div className="flex items-center gap-2">
        <Button type="button" variant="outline" disabled={!canGoBack || locked} onClick={onBack}>
          Back
        </Button>
        {isLastBuiltStep ? null : (
          <Button type="button" disabled={blocked || locked} onClick={onContinue}>
            Continue
          </Button>
        )}
      </div>
      {isLastBuiltStep ? (
        <p className="text-xs text-[var(--color-muted-foreground)]">
          This is as far as the wizard goes today. Steps 7 to 10 -- authorizing the client,
          confirming it reached us, and a first read to prove it works -- are not built yet, so
          there is no Continue here rather than a button that would lead nowhere.
        </p>
      ) : null}
    </div>
  );
}

function StepRail({
  walk,
  cursorPos,
  currentGate,
}: {
  /** The steps this wizard walks, from `walkFor`. */
  walk: readonly [Step, number][];
  /** Where the operator is in that walk, already clamped by the wizard. */
  cursorPos: number;
  /**
   * The current step's gate, from the one `stepGate` call the wizard made --
   * never a narrower re-derivation here. It is what turns the segment the
   * operator is standing on into its blocked appearance, and it is the same
   * value the Continue button is disabled from.
   */
  currentGate: StepGate;
}) {
  // THE OTHER HALF OF "ONE ROW THAT SCROLLS". Ten segments do not fit on a
  // phone, so on a narrow screen the step the operator is actually on can sit
  // off the right-hand edge -- a rail whose current step is invisible is no
  // better than the paragraph this replaced.
  //
  // THE RAIL'S OWN CONTAINER IS SCROLLED, AND NOTHING ELSE IS. `scrollIntoView`
  // is the obvious call and it is the wrong one: it moves whatever ancestors it
  // must to satisfy the request, and `block: "nearest"` does not save this
  // rail, which sits at the top of a long page. An operator ticking
  // capabilities near the bottom, whose action changes the current step, is
  // dragged back up to a rail they were not looking at. Setting `scrollLeft`
  // from the segment's own offset means the horizontal axis is the only one
  // that can move, because it is the only one written.
  const railScroller = useRef<HTMLOListElement | null>(null);
  const currentSegment = useRef<HTMLLIElement | null>(null);
  // ONE NUMBER, AND EVERY SEGMENT ASKS "AM I THAT ONE". The wizard has already
  // clamped the cursor to the first step whose gate refuses, so there is
  // nothing left for this component to override: the two blocking flags and
  // the effectiveCurrentSpec computed from them are gone, and with them the
  // case where both were true at once and two segments claimed the position.
  // The clamp makes that unconstructible rather than merely guarded against.
  const effectiveCurrentSpec = walk[cursorPos]![1];
  useEffect(() => {
    const rail = railScroller.current;
    const segment = currentSegment.current;
    if (rail === null || segment === null) return;
    // Centre the segment in the rail's own viewport, clamped to the scrollable
    // range so the first and last steps sit flush rather than half off.
    const centred = segment.offsetLeft + segment.offsetWidth / 2 - rail.clientWidth / 2;
    const left = Math.max(0, Math.min(centred, rail.scrollWidth - rail.clientWidth));
    // A motion preference is a real answer about how a person's body responds
    // to movement, so smooth is the default and never the only option. jsdom
    // implements neither `matchMedia` nor element scroll methods, hence both
    // guards: without them the unit suite throws on browser APIs that are
    // simply absent there, and `scrollLeft` is the assignment that still works
    // when `scrollTo` is missing.
    const reduceMotion =
      typeof window.matchMedia === "function" &&
      window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    if (typeof rail.scrollTo === "function") {
      rail.scrollTo({ left, behavior: reduceMotion ? "auto" : "smooth" });
    } else {
      rail.scrollLeft = left;
    }
  }, [effectiveCurrentSpec]);
  return (
    <div className="space-y-2">
      {/* THE STEPPER, AS THE DECK DRAWS IT: numbered circles, connector lines,
          done / current / upcoming states, and the 640px breakpoint that
          shortens the connectors. What shipped instead was the ten long frame
          titles run together with slashes, which is a paragraph doing a rail's
          job -- and it printed the FRAME HEADING text, which ruling 15 assigns
          to the screen heading, in the one place the short labels are
          canonical. Both halves are corrected here. */}
      {/* ONE ROW, ALWAYS, SCROLLED RATHER THAN WRAPPED -- and that is the fix
          for the orphaned connectors, not a styling preference. With
          `flex-wrap`, a connector travels with the step that follows it, so
          the first step on every row after the first opened with a line
          coming out of empty space. CSS gives a flex item no way to know it
          begins a visual row, so no "hide it at the start of a row" rule is
          writable; the alternatives are a fixed grid (which would fight the
          deck's variable-width labels) or drawing the connector as a trailing
          element (which moves the same orphan to the END of every row and
          leaves it pointing at nothing). Removing wrapping removes the whole
          class of defect instead of relocating it: ten segments stay on one
          line at every width, and the line scrolls when it does not fit.
          `snap` keeps a partly-scrolled rail landing on a segment rather than
          mid-label. */}
      <ol
        aria-label="Connection setup steps"
        ref={railScroller}
        data-testid="step-rail"
        className="mb-1 flex snap-x snap-mandatory flex-nowrap items-center overflow-x-auto pb-1"
      >
        {SPEC_STEPS.map((s, i) => {
          // THE ONE QUESTION, ASKED ONCE PER SEGMENT: am I the step the rail
          // is pointing at? `effectiveCurrentSpec` is a single number and
          // SPEC_STEPS holds each `n` exactly once, so at most one segment can
          // answer yes. Nothing else may promote a segment to current -- in
          // particular the blocking flags must not, because two of them can be
          // true at the same moment (an operator who has chosen no sites AND
          // no capabilities blocks on both), and a rail with two current steps
          // has no answer to "where am I".
          const isCurrentStep = s.n === effectiveCurrentSpec;
          // Completed means "earlier in the WALK this operator is actually
          // taking," never "a smaller specified number" -- see the comment on
          // BUILT_ORDER above for why those disagree here. The walk is already
          // clamped to the first blocked step, so nothing before `cursorPos`
          // can be blocked: "never complete while the action it gates is
          // blocked" holds structurally, with no second condition to keep in
          // step with the first.
          const walkPos = walk.findIndex(([, spec]) => spec === s.n);
          const isCompleted = walkPos !== -1 && walkPos < cursorPos;
          const availability = specStepAvailability(s);
          // SITE SCOPE'S AND THE CAPABILITY PICKER'S OWN STATES, CHECKED
          // BEFORE THE GENERIC ONES BELOW AND OVERRIDING THEM. "completed"
          // here would be the exact defect this whole rework exists for: a
          // step reading as finished while mint would still refuse it. Each
          // reason is kept distinct rather than collapsing into one muted
          // "not done yet" -- an operator told nothing when a read has
          // actually failed keeps waiting for a state that is never coming,
          // and one told "loading" for their own unmade selection is told
          // something false about themselves.
          // THE CURRENT STEP'S OWN READINESS COMES FROM THE ONE GATE, and it
          // overrides "current" only when that gate refuses. Each reason stays
          // distinct rather than collapsing into one muted "not done yet" --
          // an operator told nothing when a read has actually failed keeps
          // waiting for a state that is never coming, and one told "loading"
          // for their own unmade selection is told something false about
          // themselves. A blocked step still holds `aria-current` below,
          // because the operator IS standing on it; what it does not get is
          // the ring or the fill, which is what the style table decides.
          const state: RailSegmentState =
            availability === "not-built"
              ? "not-built"
              : isCurrentStep
                ? currentGate.refusal !== null
                  ? currentGate.readiness
                  : "current"
                : isCompleted
                  ? "completed"
                  : "upcoming";
          // EVERY VISUAL FROM THE ONE STATE VALUE, VIA THE TABLE. No style
          // reads a second boolean, so the ring cannot appear on a step whose
          // state says its action is blocked: a rail that presents a step as
          // in hand while the mint button refuses it is this component's
          // recurring defect, and one input is what makes it inexpressible.
          const style = RAIL_SEGMENT_STYLES[state];
          return (
            <li
              key={s.n}
              // The ref goes on whichever segment the rail is pointing at,
              // blocked or not: the operator needs to see the step they are
              // held on just as much as one they have finished.
              ref={isCurrentStep ? currentSegment : undefined}
              className="flex items-center"
            >
              {i > 0 ? (
                // THE CONNECTOR, AND THE DECK'S OWN BREAKPOINT. 44px wide
                // above 640px, 16px below it, margins shrinking with it. `sm:`
                // IS that 640px boundary: Tailwind's default `sm` is 640px and
                // globals.css never redefines it, which the stepper test
                // asserts rather than assumes.
                <span
                  aria-hidden="true"
                  data-testid={`step-line-${s.n}`}
                  className="mx-1.5 h-px w-4 shrink-0 bg-[var(--color-border)] sm:mx-2.5 sm:w-11"
                />
              ) : null}
              <span
                aria-current={isCurrentStep ? "step" : undefined}
                // A plain data attribute rather than only a class, so a test
                // can assert the state this rail believes it is in without
                // coupling to Tailwind class names.
                data-step-n={s.n}
                data-step-state={state}
                className="flex snap-start items-center gap-2"
              >
                <span
                  data-testid={`step-circle-${s.n}`}
                  className={cn(
                    "inline-flex size-[22px] flex-none items-center justify-center rounded-full border-[1.5px] border-[var(--color-border)] font-mono text-[11px] leading-none text-[var(--color-muted-foreground)]",
                    style.circle,
                  )}
                >
                  {s.n}
                </span>
                <span
                  data-testid={`step-label-${s.n}`}
                  className={cn(
                    "whitespace-nowrap text-[12.5px] text-[var(--color-muted-foreground)]",
                    style.label,
                  )}
                >
                  {s.rail}
                  {style.suffix}
                  {availability === "not-built" ? (
                    <span className="sr-only"> (not yet available)</span>
                  ) : null}
                </span>
              </span>
            </li>
          );
        })}
      </ol>
      {/* WHAT THE RAIL PROMISES, said once under it. Six of the ten segments
          have a section behind them; the other four do not yet, and a rail
          that showed only what is built would show an operator six tenths of a
          path and let them believe it was the whole of it. */}
      <p className="text-xs text-[var(--color-muted-foreground)]">
        Steps 1 to 6 are built and you walk them in order. Steps 7 to 10 are not built yet, so
        they are shown but not offered.
      </p>
    </div>
  );
}

/**
 * Which recommendation badge, if any, belongs on one auth card.
 *
 * COMPUTED FROM THE CLIENT TABLE, NEVER WRITTEN PER CLIENT. The wireframe
 * writes three different badges on three different frames; all three are the
 * same fact stated for a different row, so they are derived from
 * availableAuthMethods rather than copied out client by client, where the
 * fourteenth client to be added would silently get none.
 *
 * A card whose method is not available gets no badge at all: it already carries
 * the client's own reason for being disabled, and a recommendation on a control
 * nobody can press is noise.
 */
export function recommendationFor(
  method: AuthMethod,
  clientName: string,
  available: readonly AuthMethod[],
): string | null {
  if (!available.includes(method)) return null;
  // One route means there is nothing to recommend BETWEEN. Saying so is more
  // useful than a recommendation, because it tells the operator the other card
  // is not a road they are declining to take.
  if (available.length === 1) return "The only route for this client";
  // Both available. Browser sign-in is the recommendation because nothing
  // secret is ever shown; the token path is not second best, it is the path CI
  // and SSH document, so it says that rather than nothing.
  return method === "oauth"
    ? `Recommended for ${clientName}`
    : "The documented headless path";
}

/**
 * ONE SECTION, NUMBERED WITH THE SPECIFIED STEP IT ANSWERS -- never with its
 * position on this page. Numbering by position puts two numbering systems on
 * one screen -- the rail calling a section step 6 while the heading over it
 * says "4. Set it up" -- and they contradict each other as soon as the page
 * order stops matching the spine, which here it deliberately does not.
 * `specN` is looked up in SPEC_STEPS, which throws for a number
 * that is not a specified step, so a section cannot be numbered off the spine
 * at all.
 *
 * THE HEADING IS THE SPEC ENTRY'S OWN `heading`, NOT A SECOND STRING PASSED IN
 * BESIDE IT. A `title` prop next to a canonical `heading` field is two places
 * one step's name is written with nothing making them agree, which is the
 * exact shape capabilities.ts documents at length as the reason its vocabulary
 * is one list -- and drift between them is precisely the defect this component
 * was reported for.
 *
 * `narrowerTitle` is the one deliberate escape, and it is visible at the call
 * site with its reason. It exists for a built section that does LESS than the
 * deck's frame title claims: specified step 2 is "Name it, pick the AI client"
 * and the built section only picks the client (the name field is still down in
 * the setup section), so printing the deck's title over it would promise a
 * field that is not on the screen. Ruling 15 says which text is canonical; it
 * does not license a heading that describes an unbuilt screen.
 */
function Section({
  specN,
  narrowerTitle,
  hint,
  children,
}: {
  specN: number;
  narrowerTitle?: string;
  hint: string;
  children: ReactNode;
}) {
  const spec = SPEC_STEPS.find((s) => s.n === specN);
  if (spec === undefined) throw new Error(`section numbered off the spine: ${specN}`);
  const title = narrowerTitle ?? spec.heading;
  return (
    <section aria-label={title} className="space-y-3">
      <div className="space-y-0.5">
        <h2 className="text-sm font-semibold text-[var(--color-foreground)]">
          {spec.n}. {title}
        </h2>
        <p className="text-xs text-[var(--color-muted-foreground)]">{hint}</p>
      </div>
      {children}
    </section>
  );
}

function ClientCard({
  client,
  selected,
  locked,
  onSelect,
}: {
  client: McpClientRow;
  selected: boolean;
  /** A mint is open. Changing the client here would unmount its panel. */
  locked: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      disabled={locked}
      aria-pressed={selected}
      className={cn(
        "flex h-full w-full flex-col items-start gap-1 rounded-lg border p-3 text-left transition-colors",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]",
        locked && "cursor-not-allowed opacity-70",
        selected
          ? "border-[var(--color-primary)] bg-[var(--color-accent)]"
          : "border-[var(--color-border)] hover:bg-[var(--color-accent)]",
      )}
    >
      <span className="flex w-full items-center justify-between gap-2">
        <span className="text-sm font-medium text-[var(--color-foreground)]">{client.name}</span>
        {selected ? (
          <Check aria-hidden="true" className="size-4 text-[var(--color-primary)]" />
        ) : null}
      </span>
      <span className="text-xs text-[var(--color-muted-foreground)]">{client.blurb}</span>
    </button>
  );
}

/**
 * One auth method card.
 *
 * A DISABLED CARD ALWAYS SAYS WHY, IN ITS OWN WORDS. There is no branch that
 * renders a bare "unavailable": `unavailable` prints the client's own reason and
 * `unverified` prints what we have not checked and when we last looked, so a
 * stale entry looks stale instead of looking permanent.
 */
function AuthCard({
  method,
  title,
  body,
  recommendation,
  availability,
  selected,
  locked,
  onSelect,
}: {
  method: AuthMethod;
  title: string;
  body: string;
  /** From recommendationFor. Null on a card nobody can press. */
  recommendation: string | null;
  availability: AuthAvailability;
  selected: boolean;
  /**
   * A mint is open. Switching method here unmounts the panel that owns the
   * request, so it is refused for as long as the request is in the air. The
   * reason is stated once at the top of the wizard rather than repeated on
   * every card, because it is one fact about the page, not four.
   */
  locked: boolean;
  onSelect: () => void;
}) {
  const disabled = availability.state !== "available" || locked;
  return (
    <button
      type="button"
      onClick={onSelect}
      disabled={disabled}
      aria-pressed={selected}
      data-method={method}
      className={cn(
        "flex h-full flex-col items-start gap-2 rounded-lg border p-4 text-left transition-colors",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]",
        disabled && "cursor-not-allowed opacity-70",
        !disabled && selected
          ? "border-[var(--color-primary)] bg-[var(--color-accent)]"
          : "border-[var(--color-border)]",
        !disabled && !selected && "hover:bg-[var(--color-accent)]",
      )}
    >
      <span className="flex w-full items-center justify-between gap-2">
        <span className="text-sm font-medium text-[var(--color-foreground)]">{title}</span>
        {availability.state === "unavailable" ? (
          <Lock aria-hidden="true" className="size-4 text-[var(--color-muted-foreground)]" />
        ) : null}
        {availability.state === "unverified" ? (
          <AlertTriangle aria-hidden="true" className="size-4 text-[var(--color-muted-foreground)]" />
        ) : null}
      </span>

      {/* THE BADGE SAYS WHICH CARD IS THE ANSWER, and the disabled card says it
          is not possible here rather than leaving "greyed out" to be read as
          "we would rather you did not". Two different facts, two different
          words. */}
      {recommendation !== null ? (
        <Badge variant="secondary">{recommendation}</Badge>
      ) : null}
      {availability.state === "unavailable" ? (
        <Badge variant="muted">not possible here</Badge>
      ) : null}

      <span className="text-xs text-[var(--color-muted-foreground)]">{body}</span>

      {availability.state === "available" ? (
        <span className="text-xs text-[var(--color-foreground)]">{availability.detail}</span>
      ) : null}

      {availability.state === "unavailable" ? (
        <span className="text-xs text-[var(--color-foreground)]">{availability.reason}</span>
      ) : null}

      {availability.state === "unverified" ? (
        <span className="flex flex-col gap-1 text-xs text-[var(--color-foreground)]">
          <span>Not yet verified by us. {availability.reason}</span>
          {/* The date is the point: it makes an entry that has gone stale look
              stale, instead of reading as a permanent property of the client. */}
          <span className="text-[var(--color-muted-foreground)]">
            Last checked {availability.lastCheckedAt}.
          </span>
        </span>
      ) : null}
    </button>
  );
}

function SnippetBlock({ client, snippet }: { client: McpClientRow; snippet: Snippet }) {
  return (
    <div className="space-y-3">
      {snippet.kind === "json" ? (
        <div className="space-y-2">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="text-xs font-medium text-[var(--color-foreground)]">
              Config for {client.name}
            </span>
            <Badge variant="muted">JSON</Badge>
          </div>
          <pre className="overflow-x-auto rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 p-3 font-mono text-xs text-[var(--color-foreground)]">
            <code>{snippet.text}</code>
          </pre>
          <div className="flex flex-wrap items-center gap-2">
            <CopyableMono value={snippet.text} label={`Copy the ${client.name} config`} truncate />
          </div>
          {/* The reason for the type line, on screen rather than in a comment. */}
          <p className="text-xs text-[var(--color-muted-foreground)]">{snippet.note}</p>
        </div>
      ) : null}

      {snippet.kind === "gui" ? (
        <div className="space-y-2">
          <span className="text-xs font-medium text-[var(--color-foreground)]">
            Set this up inside {client.name}
          </span>
          <ol className="list-decimal space-y-1 pl-5 text-sm text-[var(--color-foreground)]">
            {snippet.steps.map((s) => (
              <li key={s}>{s}</li>
            ))}
          </ol>
          <CopyableMono value={snippet.url} label="Copy the endpoint" />
        </div>
      ) : null}

      {snippet.kind === "raw" ? (
        <div className="space-y-2">
          <span className="text-xs font-medium text-[var(--color-foreground)]">
            Endpoint for {client.name}
          </span>
          <p className="text-sm text-[var(--color-muted-foreground)]">{snippet.reason}</p>
          <CopyableMono value={snippet.url} label="Copy the endpoint" />
          {snippet.headerLine !== null ? (
            <CopyableMono value={snippet.headerLine} label="Copy the authorization header" />
          ) : null}
        </div>
      ) : null}

      {snippet.kind === "shell" ? (
        <div className="space-y-2">
          <span className="text-xs font-medium text-[var(--color-foreground)]">
            Run this in a terminal for {client.name}
          </span>
          <p className="text-sm text-[var(--color-muted-foreground)]">{snippet.reason}</p>
          <pre className="overflow-x-auto rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 p-3 font-mono text-xs text-[var(--color-foreground)]">
            <code>{snippet.text}</code>
          </pre>
          <CopyableMono value={snippet.text} label={`Copy the ${client.name} setup command`} truncate />
          {/* The mechanic, on screen rather than only in the generator's own
              comment: a reader has to be able to tell this is safe without
              reading snippet.ts. */}
          <p className="text-xs text-[var(--color-muted-foreground)]">
            Reads the token once, into your shell, and never types it as an argument -- so it is
            never echoed and never written to your shell history.
          </p>
        </div>
      ) : null}

      {/* The endpoint just printed is derived from this origin, which does not
          prove anything forwards it. Said once, beside every artefact. */}
      <p className="text-xs text-[var(--color-muted-foreground)]">
        {SELF_HOSTED_PROXY_REQUIREMENT}
      </p>

      <p className="text-xs text-[var(--color-muted-foreground)]">
        {/* Stated as data from the table, so it reads as "we have not checked"
            rather than "this client has no config file". */}
        {CONFIG_PATH_GAP}{" "}
        <a
          href={client.docsUrl}
          target="_blank"
          rel="noreferrer noopener"
          className="inline-flex items-center gap-1 text-[var(--color-primary)] underline-offset-4 hover:underline"
        >
          {client.docsLabel}
          <ExternalLink aria-hidden="true" className="size-3" />
        </a>
      </p>
    </div>
  );
}

function NextSteps({ clientName }: { clientName: string }) {
  return (
    <div className="rounded-md border border-[var(--color-border)] p-3 text-sm text-[var(--color-foreground)]">
      <p className="font-medium">What happens next</p>
      <p className="mt-1 text-[var(--color-muted-foreground)]">
        Start the connection in {clientName}. It sends you back here to approve, and that approval
        screen is where you choose which sites it may touch and what it may do. Nothing is created
        until you approve.
      </p>
    </div>
  );
}

/** The site-scope fields a mint request carries, exactly as they go on the wire. */
interface MintScope {
  readonly siteScopeMode: SiteScopeSelection["mode"];
  readonly scopeTagIds: readonly string[];
  readonly scopeSiteIds: readonly string[];
}

type MintScopeRequest =
  | { readonly ok: true; readonly scope: MintScope }
  | {
      readonly ok: false;
      readonly because: "tags-unresolved" | "names-nothing";
      readonly refusal: string;
    };

/**
 * The site-scope payload for the CURRENT selection, or the reason there is none.
 *
 * THE GATE AND THE REQUEST ARE THE SAME VALUE. They used to be two derivations
 * of one selection, and they disagreed: the gate asked `isScopeApprovable`,
 * which says an empty scope is a working state (site-scope.ts, the 2026-08-23
 * wireframe revision), while the request sent whatever was in state. So a
 * button the screen had enabled produced a 400 the operator could do nothing
 * with. Returning the payload and the refusal from one function means an
 * enabled button always has a payload, and it is that payload that is sent.
 *
 * TWO THINGS THE SERVER REFUSES, AND THE SCREEN MUST NOT SEND:
 *
 *   NAMING NOTHING. ValidateSiteScopeRequest (apps/api/internal/mcp/scope.go
 *   lines 168-200) refuses mode 'tags' with no tag ids and mode 'list' with no
 *   site ids, both with the same reason: "an empty tag list grants nothing and
 *   is not a way to request every site". The wizard OPENS on mode 'list' with
 *   nothing selected, so this was reachable by doing nothing at all: pick a
 *   client, pick the token method, press the button, get a 400.
 *
 *   NAMING THE WRONG KIND. The same function refuses mode 'all' that also
 *   carries tags or sites, and mode 'tags' that also carries sites. Step 3
 *   deliberately KEEPS the other mode's selection when the segmented control
 *   is flipped (site-scope-step.tsx: "a mode flipped by accident does not throw
 *   away the other answer"), which is right for the picker and wrong for the
 *   wire. Ticking three sites and then choosing "All sites" sent mode 'all'
 *   with three site ids. Only the ids belonging to the live mode are carried.
 */
function mintScopeRequest(
  mode: SiteScopeSelection["mode"],
  scopeTagIds: readonly string[] | null,
  scopeSiteIds: readonly string[],
): MintScopeRequest {
  if (mode === "all") {
    return { ok: true, scope: { siteScopeMode: "all", scopeTagIds: [], scopeSiteIds: [] } };
  }
  if (mode === "tags") {
    if (scopeTagIds === null) {
      return {
        ok: false,
        because: "tags-unresolved",
        refusal:
          "This connection's tag scope could not be resolved to ids -- the tag registry has not finished loading, or a tag you picked is no longer in it. Reopen step 3, confirm your tags once it loads, and try again.",
      };
    }
    if (scopeTagIds.length === 0) {
      return {
        ok: false,
        because: "names-nothing",
        refusal:
          "This connection is scoped by tag and no tag is picked, so there is nothing to mint it against. Pick at least one tag in step 3, or switch that step to All sites. An empty tag list is refused rather than read as every site.",
      };
    }
    return { ok: true, scope: { siteScopeMode: "tags", scopeTagIds, scopeSiteIds: [] } };
  }
  if (scopeSiteIds.length === 0) {
    return {
      ok: false,
      because: "names-nothing",
      refusal:
        "This connection is scoped to named sites and no site is picked, so there is nothing to mint it against. Pick at least one site in step 3, or switch that step to All sites. An empty list is refused rather than read as every site.",
    };
  }
  return { ok: true, scope: { siteScopeMode: "list", scopeTagIds: [], scopeSiteIds } };
}

/**
 * Whether step 3 is actually done, for every reason mint can refuse it.
 *
 * IT NO LONGER ASKS WHICH AUTH METHOD WAS CHOSEN, BECAUSE AT STEP 3 THERE IS
 * NOT ONE. This function used to answer "resolved" for anything but the token
 * path, on the reasoning that the browser sign-in path has no mint button for
 * an unanswered scope to refuse. That reasoning depended entirely on the auth
 * step coming SECOND. In the specified order it comes fifth, so at step 3 the
 * method is always null, and keeping the exemption would have waved every
 * operator past this step and then refused them at step 6 with a sentence
 * about step 3 -- a refusal pointing backwards at a step they were told was
 * settled.
 *
 * SO THE GATE IS THE ANSWER ITSELF, ALWAYS. What blocks:
 *     unselected      -> mintScopeRequest: "names-nothing"
 *     loading         -> scope.kind === "unresolved"
 *     failed          -> scope.kind === "unresolved"
 *     tags-unresolved -> mintScopeRequest: "tags-unresolved"
 *     resolved        -> not blocked (the name field is a separate, later
 *                        gate -- see the note on that below)
 *
 * "All sites" is the one-click answer for every one of them, so this is a
 * question with an always-available answer rather than a wall.
 *
 * WHAT THIS COSTS THE BROWSER SIGN-IN PATH, SAID PLAINLY: an operator on that
 * path must now answer step 3 to advance, and their answer is still rehearsal
 * -- the client opens the approval screen itself and it asks again, which
 * SiteScopeStep's own copy says on the step. Asking for an answer that is
 * carried by the operator rather than by the wire is what this step has always
 * been on that path; the change is that it is asked before it is possible to
 * know the path.
 *
 * THE ONE PLACE THIS IS DECIDED. `mintBlockedReason` below and the step rail's
 * `siteScopeState` (ConnectWizard) both call this and nothing else, so
 * "minting is blocked" and "the rail says step 3 is done" cannot disagree --
 * they read the same value rather than two derivations that could drift the
 * way `scope.kind` alone already did once.
 *
 * THE ORDER ON THE TOKEN PATH IS LOAD-BEARING, copied from
 * `mintBlockedReason`'s own comment because this function replaces that logic
 * rather than duplicating it. An unresolved tag registry is reported first,
 * because a selection that cannot be translated is not the same complaint as
 * a selection that is empty. A fleet still loading is reported before "you
 * have picked nothing," because telling an operator to pick a site out of a
 * list that has not arrived is a remedy they cannot follow.
 *
 * A NAMED NON-GOAL, NOT AN OVERSIGHT. The connection-name field (inside step
 * 6 itself) can be empty while this returns `resolved` and the rail marks
 * step 6 current -- mint is still blocked then, by `mintBlockedReason`'s own
 * `!nameOk` branch, which this function does not see. That is correct rather
 * than a fifth gap to close: the rail reports POSITION -- which section is
 * the operator working in -- not "is the submit button enabled right now."
 * An empty name field is the operator genuinely inside step 6, working on
 * step 6's own control, not the rail lying about where they are the way the
 * site-scope cases above did.
 */
function siteScopeReadiness(
  scope: ResolvedSiteScope,
  scopeRequest: MintScopeRequest,
): SiteScopeReadiness {
  if (!scopeRequest.ok && scopeRequest.because === "tags-unresolved") return "tags-unresolved";
  if (scope.kind === "unresolved") return scope.because;
  if (!scopeRequest.ok) return "unselected";
  return "resolved";
}

/** The capability payload a mint would send, or the reason there is none. */
type MintCapabilitiesRequest =
  | { readonly ok: true; readonly capabilities: readonly string[] }
  | { readonly ok: false; readonly refusal: string };

/**
 * The capability payload for the CURRENT selection, or the reason there is
 * none -- the same shape and the same reason `mintScopeRequest` above returns
 * one, so the gate (`mintBlockedReason`) and the wire payload
 * (`TokenMintPanel`'s mint call) read one value rather than two derivations
 * of the same checkbox state.
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
function mintCapabilitiesRequest(selected: readonly string[]): MintCapabilitiesRequest {
  if (selected.length === 0) {
    return {
      ok: false,
      refusal:
        "No capability is selected, so this token would authenticate and be able to reach nothing. Pick at least one capability above, or leave Sites checked. An empty selection is refused rather than becoming the default.",
    };
  }
  return { ok: true, capabilities: selected };
}

/**
 * Whether the capability picker is actually done, mirroring
 * `siteScopeReadiness` immediately above and, like it, no longer asking which
 * auth method was chosen: step 4 is asked before step 5, so there is never one
 * to ask about.
 *
 * The picker opens with `mcp.sites.read` ticked, so an operator who reads this
 * step and changes nothing is settled. The only way to block here is to untick
 * everything, which is the one selection the server would accept and the
 * operator would not want -- a token that authenticates and can reach nothing.
 */
function capabilityReadiness(
  capabilitiesRequest: MintCapabilitiesRequest,
): CapabilityReadiness {
  return capabilitiesRequest.ok ? "resolved" : "unselected";
}

/**
 * The reason minting is refused, or null when it is not.
 *
 * Every branch here is a FAILURE, rendered with a remedy, never a silently
 * disabled button.
 */
/** Everything any step's gate can need. One value, so no caller assembles its own. */
interface StepGateContext {
  readonly clientChosen: boolean;
  readonly method: AuthMethod | null;
  readonly methodOffered: boolean;
  readonly scope: ResolvedSiteScope;
  readonly scopeRequest: MintScopeRequest;
  readonly capabilitiesRequest: MintCapabilitiesRequest;
}

/**
 * Whether one local step is settled, and the reason it is not.
 *
 * THE SINGLE PREDICATE. The rail's "is this segment done", the Continue
 * button's "may the operator advance", and the mint button's "will the server
 * accept this" are all THE SAME QUESTION and are answered here, once. This
 * component's history is call sites re-deriving a narrower version of this and
 * disagreeing with each other. Adding a separate notion of "may I advance" for
 * Continue would be the next instance, which is why Continue reads this and
 * there is nothing else for it to read.
 *
 * INVARIANT, and the reason the two fields travel together in one return
 * rather than in two functions: `refusal === null` exactly when
 * `readiness === "resolved"`. A caller can branch on either and cannot get a
 * different answer from the other.
 *
 * The readiness strings are deliberately the SiteScopeReadiness vocabulary
 * reused whole, so RAIL_SEGMENT_STYLES already carries a style and a suffix
 * for every value a step can be blocked on, with no new row for steps 1 and 2:
 * an operator who has not picked a client is in exactly the same "not chosen
 * yet" state as one who has not picked a site.
 */
interface StepGate {
  readonly readiness: SiteScopeReadiness;
  readonly refusal: string | null;
}

const STEP_SETTLED: StepGate = { readiness: "resolved", refusal: null };

function stepGate(local: Step, ctx: StepGateContext): StepGate {
  // THE CONTRACT STEP ASKS NOTHING, SO IT REFUSES NOTHING. It is a reading
  // step: what a connection can do, what it can never do, and that nothing
  // about it is implicit. A gate here would be a control demanding an answer
  // to a question that was not asked.
  if (local === CONTRACT_LOCAL_STEP) return STEP_SETTLED;
  if (local === CLIENT_LOCAL_STEP) {
    return ctx.clientChosen
      ? STEP_SETTLED
      : { readiness: "unselected", refusal: "Pick the client that will connect before going on." };
  }
  if (local === AUTH_LOCAL_STEP) {
    if (ctx.method !== null) return STEP_SETTLED;
    return {
      readiness: "unselected",
      refusal: ctx.methodOffered
        ? "Choose how this client signs in before going on."
        : "This client has no confirmed way to sign in, so there is nothing to go on to. Go back to step 2 and pick a different client.",
    };
  }
  if (local === SITE_SCOPE_LOCAL_STEP) {
    const readiness = siteScopeReadiness(ctx.scope, ctx.scopeRequest);
    if (readiness === "resolved") return STEP_SETTLED;
    if (readiness === "loading") {
      return {
        readiness,
        refusal:
          "Still reading this organisation's sites for step 3. Wait for that to finish before minting.",
      };
    }
    if (readiness === "failed") {
      return {
        readiness,
        refusal:
          "This organisation's sites could not be read for step 3, so we cannot tell what this connection would cover. Fix that above before minting.",
      };
    }
    // "tags-unresolved" and "unselected" are the only readiness values left,
    // and siteScopeReadiness reaches both only through scopeRequest refusing,
    // so it always carries a remedy here. Reusing that sentence, rather than
    // writing a second copy, is what keeps the gate and the refusal text from
    // drifting apart. The `ok` branch is unreachable and is written as a throw
    // rather than a silent pass, because a silent pass would be the gate
    // approving a step it has just called unresolved.
    if (ctx.scopeRequest.ok) {
      throw new Error(`site scope is "${readiness}" but the request has no refusal`);
    }
    return { readiness, refusal: ctx.scopeRequest.refusal };
  }
  if (local === CAPABILITY_LOCAL_STEP) {
    const readiness = capabilityReadiness(ctx.capabilitiesRequest);
    if (readiness === "resolved") return STEP_SETTLED;
    return {
      readiness,
      refusal: ctx.capabilitiesRequest.ok ? null : ctx.capabilitiesRequest.refusal,
    };
  }
  // The setup artefact. Nothing gates LEAVING it, because nothing after it is
  // built -- see the terminus panel it renders instead of a Continue.
  return STEP_SETTLED;
}

function mintBlockedReason(nameOk: boolean, ctx: StepGateContext): string | null {
  if (!nameOk) return "Name this connection before minting a token.";
  // Site scope is checked before capabilities, matching the on-screen order
  // (step 3, then step 4): an operator missing both should be told about the
  // earlier gap first, not the later one.
  const scopeGate = stepGate(SITE_SCOPE_LOCAL_STEP, ctx);
  if (scopeGate.refusal !== null) return scopeGate.refusal;
  return stepGate(CAPABILITY_LOCAL_STEP, ctx).refusal;
}

/** Title and remedy for a failed mint. Every branch is distinct and named. */
function mintErrorCopy(error: Error): { title: string; body: string } {
  if (!(error instanceof ConnectionsRequestError)) {
    // A raw fetch failure -- offline, DNS, a dropped connection -- never
    // reaches readHouseError, so it is never a ConnectionsRequestError. It is
    // still a failure and still gets a remedy, not the fact-shaped silence of
    // an empty catch.
    return {
      title: "The request did not reach the server",
      body: "Check your connection and try again. Nothing was created.",
    };
  }
  if (error.status === 403) {
    return {
      title: "Your role cannot mint a connection token",
      body: "An AI connection is an organisation-wide credential, so minting one needs full organisation membership, not access to one site. Ask an organisation admin to mint it, or ask them to raise your role.",
    };
  }
  if (error.status === 429) {
    const retry =
      typeof error.details?.retry_after_seconds === "number"
        ? error.details.retry_after_seconds
        : null;
    return {
      title: "Too many connection tokens minted recently",
      body:
        retry !== null
          ? `Wait about ${retry} second${retry === 1 ? "" : "s"} and try again.`
          : "Wait a short while and try again.",
    };
  }
  if (error.code === "mcp_unknown_scope_tag" || error.code === "mcp_unknown_scope_site") {
    return {
      title: "A site or tag from step 3 no longer exists",
      body: "Something in the scope you picked was deleted since step 3 loaded. Reopen step 3, re-pick sites or tags, and try again.",
    };
  }
  return { title: "The server refused this request", body: error.message };
}

/**
 * A minted token together with the configuration it was minted FOR.
 *
 * The three fields travel as one value on purpose. Splitting the token from
 * its scope is what let the reveal pair a real secret with a description read
 * from state that had moved on; keeping them in one object means the reveal
 * cannot be handed a token without also being handed the scope that token
 * actually carries.
 */
type MintedReveal = {
  readonly token: MintedConnection;
  readonly scope: ResolvedSiteScope;
  readonly clientName: string;
};

/**
 * Step 6's one-time reveal, and the button that produces it (design §S29,
 * revision 2026-08-24; the wireframe's own header badge marks this the
 * governing generation).
 *
 * THE TOKEN LIVES ONLY IN THIS COMPONENT'S OWN STATE. It is never handed to
 * TanStack Query's cache (useMintConnection.onSuccess invalidates the list, it
 * does not setQueryData the token), so there is no cache key that could hand
 * it back. Leaving this step -- picking a different client or method, which
 * unmounts this panel, or navigating away from the route entirely -- drops the
 * state with it. Editing an input THIS panel depends on without leaving also
 * clears it, below, so a revealed token can never be shown beside a
 * configuration it was not actually minted for.
 *
 * THE REVEAL IS BUILT FROM THE REQUEST, NOT FROM LIVE STATE. Clearing on edit
 * cannot cover the in-flight window: a mint started against one configuration
 * and finishing after the operator edited the name or the scope arrived at an
 * `onSuccess` that fired unconditionally, and the reveal then described the
 * scope it read at that moment -- a real token beside access it does not
 * carry, on the one screen whose whole job is saying what was just made, at
 * the only moment the token is ever visible. `MintedReveal` snapshots the
 * scope and client the request was sent with and stores them WITH the
 * response, so the reveal has no live-state input at all to be stale about.
 *
 * DISCARDING THE STALE MINT WOULD HAVE BEEN THE OTHER FIX, AND IS WORSE: the
 * server has already created that credential. Refusing to show it leaves a
 * live token in the organisation that nobody holds and nobody was told to
 * revoke, which trades a mislabelled secret for an unaccounted one. Labelling
 * it correctly costs nothing and loses nothing.
 */
function TokenMintPanel({
  mint,
  revealed,
  onMinted,
  clientId,
  clientName,
  name,
  scope,
  scopeRequest,
  capabilitiesRequest,
}: {
  /**
   * The mutation, OWNED BY THE WIZARD. This panel drives it and reads it, and
   * deliberately does not hold it: a mutation whose lifetime is this
   * component's lifetime is a request that dies when the operator changes the
   * method, which is the whole defect.
   */
  mint: UseMutationResult<MintedConnection, Error, MintConnectionInput>;
  /** A token is already on screen, rendered by the wizard above every step. */
  revealed: boolean;
  onMinted: (reveal: MintedReveal) => void;
  /**
   * The client table's `id`, which is what goes on the wire as `setup_client`.
   * Held separately from `clientName` rather than derived from it: the server
   * refuses anything but the slug shape, and "Claude Code" is not one.
   */
  clientId: string;
  clientName: string;
  name: string;
  scope: ResolvedSiteScope;
  scopeRequest: MintScopeRequest;
  /** From `mintCapabilitiesRequest`, built once by the wizard -- see its doc. */
  capabilitiesRequest: MintCapabilitiesRequest;
}) {
  const trimmedName = name.trim();
  const nameOk = trimmedName.length > 0;
  // Literally "token", not threaded through as a parameter: this panel is only
  // rendered for method === 'token', so the context is a fact about the caller
  // rather than a value to plumb. clientChosen and methodOffered are true for
  // the same reason -- this panel cannot render otherwise.
  const blocked = mintBlockedReason(nameOk, {
    clientChosen: true,
    method: "token",
    methodOffered: true,
    scope,
    scopeRequest,
    capabilitiesRequest,
  });
  const canMint = blocked === null && !mint.isPending;

  if (revealed) {
    // The reveal itself is rendered at the top of the wizard, not here, so
    // that no step condition governs whether a live credential is visible.
    // What belongs here is a pointer to it, because an operator who scrolled
    // to step 4 to press a button must not find the button simply gone.
    return (
      <p className="text-sm text-[var(--color-muted-foreground)]">
        This connection's token is shown at the top of this page. It is shown once and is not
        shown again, so copy it before you dismiss it.
      </p>
    );
  }

  return (
    <div className="space-y-3">
      <p className="text-sm text-[var(--color-muted-foreground)]">
        Mint a connection token for {clientName}. It is shown once, here, and never again.
      </p>

      {blocked !== null ? (
        <p
          role="alert"
          className="rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 p-3 text-sm text-[var(--color-foreground)]"
        >
          {blocked}
        </p>
      ) : null}

      {mint.isError
        ? (() => {
            const { title, body } = mintErrorCopy(mint.error);
            return (
              <div
                role="alert"
                className="rounded-md border border-[var(--color-destructive)]/40 bg-[var(--color-destructive)]/5 p-3 text-sm"
              >
                <p className="font-medium text-[var(--color-foreground)]">{title}</p>
                <p className="mt-1 text-[var(--color-muted-foreground)]">{body}</p>
              </div>
            );
          })()
        : null}

      <Button
        type="button"
        disabled={!canMint}
        onClick={() => {
          // canMint already proved this, and an enabled button with no payload
          // would be the gate and the request disagreeing again. Refusing here
          // rather than sending `?? []` / `?? ["mcp.sites.read"]` keeps that
          // impossible instead of quiet -- see mintCapabilitiesRequest's doc
          // for why an empty selection must never reach the wire as `[]`.
          if (!scopeRequest.ok) return;
          if (!capabilitiesRequest.ok) return;
          // Read here, at the moment the request is built, and closed over.
          // Reading them again inside onSuccess is the whole defect: that runs
          // after a round trip the operator can type through.
          const mintedFor = { scope, clientName };
          mint.mutate(
            {
              name: trimmedName,
              ...scopeRequest.scope,
              capabilities: capabilitiesRequest.capabilities,
              setupClient: clientId,
            },
            // onMinted writes state the WIZARD owns, so this callback lands on
            // a mounted component whatever happened to this panel meanwhile.
            { onSuccess: (token) => onMinted({ token, ...mintedFor }) },
          );
        }}
      >
        {mint.isPending ? "Minting..." : "Generate connection token"}
      </Button>
    </div>
  );
}

/**
 * The one-time reveal itself. Rendered exactly once per mint, from state the
 * parent never repopulates -- see TokenMintPanel's own doc.
 */
function TokenReveal({ reveal, onDismiss }: { reveal: MintedReveal; onDismiss: () => void }) {
  // Destructured from the one snapshot, and there is deliberately no second
  // source: this component takes no live prop it could describe the token
  // with, so a stale label is not a race that has to be won but a state that
  // cannot be constructed.
  const { clientName, scope, token } = reveal;
  const capabilities =
    token.capabilities.length > 0 ? token.capabilities.join(", ") : "no capabilities";
  return (
    <div className="space-y-3">
      <div
        role="alert"
        className="rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 p-3 text-sm"
      >
        <p className="flex items-center gap-1.5 font-medium text-[var(--color-foreground)]">
          <AlertTriangle aria-hidden="true" className="size-4" />
          This is the only time this token is shown
        </p>
        <p className="mt-1 text-[var(--color-muted-foreground)]">
          We store a hash, not the token. Copy it into your password manager now, or rotate the
          connection later and reconfigure {clientName}. There is no showing it again.
        </p>
      </div>

      <div className="rounded-lg border border-[var(--color-border)] p-3">
        <div className="flex items-center justify-between gap-2">
          <span className="text-xs font-medium text-[var(--color-foreground)]">
            Your connection token
          </span>
          <Badge variant="muted">Shown once</Badge>
        </div>
        <div className="mt-2">
          {/* NOT truncate. This is the one and only time the operator sees this
              value; middle-truncating it here would hide characters they may
              need to visually verify against what lands in their password
              manager, for a secret that cannot be looked up again to check. */}
          <CopyableMono value={token.token} label="Copy the connection token" />
        </div>
        <p className="mt-2 text-xs text-[var(--color-muted-foreground)]">
          {describeSiteScope(scope)} Capabilities: {capabilities}. Revocable from the connections
          list, immediately, at the next request.
        </p>
        {/* THE HANDLE FOR REVOKING IT, shown beside the secret rather than
            only in a list the operator has not opened. If anything goes wrong
            between here and their password manager, this prefix is what lets
            them find and kill this exact credential; without it, "revoke the
            one I just made" is a guess. It is not the secret and is safe to
            leave on screen. */}
        {/* EXPIRY IS FOR A PERSON TO READ, so it is not the wire format. This
            printed `expiresAt` raw and put "2026-09-06T12:00:00Z" inside an
            English sentence, on the one screen the operator reads once and
            cannot come back to. formatAbsolute is the same helper the update
            runs use and it always names the zone it resolved the time in: an
            expiry without a zone is not an answer to "when does this stop
            working", and this token's whole value is knowing that. */}
        <p className="mt-1 text-xs text-[var(--color-muted-foreground)]">
          Listed as <span className="font-mono">{token.tokenPrefix}</span> in the connections
          list. Expires {formatAbsolute(token.expiresAt)}.
        </p>
      </div>

      {/* DISMISSAL IS AN ACT, NEVER A SIDE EFFECT. This panel used to clear
          itself whenever an input it was minted for changed, which meant one
          keystroke in the name field destroyed the only copy of a live
          credential -- the same accounting hole as losing the response to an
          unmount, reached by a shorter route. The reveal carries the
          configuration it was minted FOR, so it cannot go stale and has
          nothing to be cleared about. */}
      <Button type="button" variant="outline" onClick={onDismiss}>
        I have saved this token
      </Button>
    </div>
  );
}
