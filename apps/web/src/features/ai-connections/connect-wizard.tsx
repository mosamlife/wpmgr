import { useMemo, useState, type ReactNode } from "react";
import type { UseMutationResult } from "@tanstack/react-query";
import { useBlocker } from "@tanstack/react-router";
import { AlertTriangle, Check, ExternalLink, Lock } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { CopyableMono } from "@/components/shared/copyable-mono";
import { cn } from "@/lib/utils";

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
  isScopeApprovable,
  resolveSiteScope,
  resolveTagIds,
  type FleetSnapshot,
  type ResolvedSiteScope,
} from "@/features/mcp-consent/site-scope";

// The connection wizard (design §18). Client first, method second.
//
// THE ORDERING IS THE DESIGN, NOT A PREFERENCE. "The user picks the client and
// the system computes which authentication methods are possible, disabling the
// rest with the specific reason written into the disabled card -- never a
// generic 'unavailable'. Asking a user to choose an auth method their client
// cannot use is asking them to fail."
//
// WHAT THIS RENDERS AND WHAT IT DOES NOT. Steps 1, 2, 3 and the setup artefact
// live here. STEP 3 IS NEW AND IS A REAL DECISION SURFACE: the wireframe puts
// "which sites" before "what it may do" on purpose, and mcp_grants already
// carries site_scope_mode, scope_tag_ids and scope_site_ids, so nothing about
// the selection is blocked on the backend.
//
// WHAT STEP 3 STILL CANNOT DO, SAID ON SCREEN RATHER THAN HIDDEN. The grant is
// created by the approval screen at /connect/ai, which the CLIENT redirects
// into; this wizard never calls it and has no channel to hand it an answer.
// So the choice made here is the operator arriving with the answer, not the
// answer being written. The same is already true of the connection name two
// sections down, and it is stated the same way rather than implied.
//
// THERE IS NO CAPABILITIES STEP HERE, AND NOT BECAUSE THE BACKEND IS MISSING.
// The vocabulary is seated (policy.go: eight capability strings, seven of
// them conferrable via the read scope), and one of this wizard's two
// completion paths already puts it on the wire: TokenMintPanel below calls
// useMintConnection(), which POSTs to CONNECTIONS_PATH and already accepts a
// `capabilities` list on the request (dto.go:264) and returns one on the
// response (dto.go:305). A picker on THAT path would need only a frontend
// field -- MintConnectionInput (use-ai-connections.ts:228-233) has name,
// siteScopeMode, scopeTagIds and scopeSiteIds, and no capabilities -- so the
// gap there is this file, not the server.
//
// THE OAUTH PATH IS THE OTHER ONE, AND IS WHERE "THIS WIZARD NEVER CREATES
// THE GRANT" (WHAT STEP 3 STILL CANNOT DO, above) IS ACTUALLY TRUE. The
// client redirects into the approval screen at /connect/ai, which calls
// Approve (service.go:607); ApprovalRequest carries Principal, Consent,
// GrantName and SiteScope and nothing else, so that path genuinely has no
// channel for a capability answer today.
//
// So a picker built only against the mint call would work for token
// connections and do nothing for OAuth ones, and a permission model that
// varies by how the connection was made is worse than one that stays
// uniformly narrow -- which is why neither path has one, rather than one
// path having one and the other not. The choice among the three real options
// is open, tracked in GH #660, and this file does not make it. Cited by
// file:line above rather than restated from memory: summarising this split
// instead of pointing at it is exactly what went stale here before.
//
// NO SNIPPET IS WRITTEN IN THIS FILE. Every block comes from buildSnippet, and
// snippet.test.ts fails the build if a config literal appears here.

type Step = 1 | 2 | 3 | 4;

interface StepDef {
  readonly n: Step;
  readonly label: string;
}

const STEPS: readonly StepDef[] = [
  { n: 1, label: "Pick your client" },
  { n: 2, label: "How it signs in" },
  { n: 3, label: "Sites it may reach" },
  { n: 4, label: "Set it up" },
];

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

  const client = useMemo(
    () => MCP_CLIENTS.find((c) => c.id === clientId) ?? null,
    [clientId],
  );

  const methods = client === null ? [] : availableAuthMethods(client);

  // The current step is derived, never stored, so it cannot disagree with the
  // answers. Picking a different client with an incompatible method drops the
  // method rather than carrying a stale one into step 3.
  const step: Step = client === null ? 1 : method === null ? 2 : 3;

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
      <StepRail current={step} />

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

      <Section
        n={1}
        title="Pick your client"
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

      {client !== null ? (
        <Section
          n={2}
          title="How it signs in"
          hint={`Computed from ${client.name}. A method it cannot use is disabled with the reason.`}
        >
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <AuthCard
              method="oauth"
              title="Sign in through your browser"
              body="You approve the connection on a WPMgr page. The client stores a token it refreshes itself, and nothing secret is ever shown to you or pasted anywhere."
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

      {client !== null && method !== null ? (
        <Section
          n={3}
          title="Sites this connection may reach"
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
          {/* The same correction as the name field below, for the same reason.
              The client opens the approval screen itself, so this page has no
              channel into it and cannot hand it a scope. Deciding it here is
              how you arrive with the answer; the approval screen is where it
              is written, and it asks again. */}
          <p className="mt-3 text-xs text-[var(--color-muted-foreground)]">
            Nothing carries this selection to the approval screen. The client opens that
            screen itself, so it asks for the scope again and that is where it is written.
            Deciding it here is how you get there with the answer ready.
          </p>
        </Section>
      ) : null}

      {client !== null && method !== null ? (
        <Section
          n={4}
          title="Set it up"
          hint="Generated for this client. Every difference below is a real difference between clients."
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
                clientName={client.name}
                name={name}
                scope={scope}
                scopeRequest={scopeRequest}
              />
            )}
          </div>
        </Section>
      ) : null}

      <p className="text-xs text-[var(--color-muted-foreground)]">
        We negotiate protocol {PROTOCOL_TARGET_VERSION} and accept nothing below{" "}
        {PROTOCOL_FLOOR_VERSION}.
      </p>
    </div>
  );
}

function StepRail({ current }: { current: Step }) {
  return (
    <ol className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-[var(--color-muted-foreground)]">
      {STEPS.map((s, i) => (
        <li key={s.n} className="flex items-center gap-2">
          {i > 0 ? <span aria-hidden="true">/</span> : null}
          <span
            aria-current={s.n === current ? "step" : undefined}
            className={cn(
              s.n === current && "font-medium text-[var(--color-foreground)]",
              s.n < current && "text-[var(--color-foreground)]",
            )}
          >
            {s.n}. {s.label}
          </span>
        </li>
      ))}
      <li className="flex items-center gap-2">
        <span aria-hidden="true">/</span>
        {/* Capabilities are NOT a numbered step in this rail. The vocabulary
            and the grant column behind them exist now (policy.go,
            mcp_grants.capabilities in schema.sql), but neither completion
            path this wizard ends in lets an operator choose one today -- see
            the comment above the wizard's STEPS export for the token/OAuth
            split -- so a step number here would be for a choice nobody can
            make yet. What it names instead is where the OAuth path's consent
            is actually recorded. */}
        <span>Permissions are chosen on the approval screen</span>
      </li>
    </ol>
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

function Section({
  n,
  title,
  hint,
  children,
}: {
  n: number;
  title: string;
  hint: string;
  children: ReactNode;
}) {
  return (
    <section aria-label={title} className="space-y-3">
      <div className="space-y-0.5">
        <h2 className="text-sm font-semibold text-[var(--color-foreground)]">
          {n}. {title}
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
 * The reason minting is refused, or null when it is not.
 *
 * Every branch here is a FAILURE, rendered with a remedy, never a silently
 * disabled button.
 *
 * THE ORDER IS LOAD-BEARING. An unresolved tag registry is reported first,
 * because a selection that cannot be translated is not the same complaint as a
 * selection that is empty. A fleet still loading is reported before "you have
 * picked nothing", because telling an operator to pick a site out of a list
 * that has not arrived is a remedy they cannot follow.
 */
function mintBlockedReason(
  nameOk: boolean,
  scope: ResolvedSiteScope,
  scopeRequest: MintScopeRequest,
): string | null {
  if (!nameOk) return "Name this connection before minting a token.";
  if (!scopeRequest.ok && scopeRequest.because === "tags-unresolved") return scopeRequest.refusal;
  if (!isScopeApprovable(scope)) {
    return scope.kind === "unresolved" && scope.because === "loading"
      ? "Still reading this organisation's sites for step 3. Wait for that to finish before minting."
      : "This organisation's sites could not be read for step 3, so we cannot tell what this connection would cover. Fix that above before minting.";
  }
  if (!scopeRequest.ok) return scopeRequest.refusal;
  return null;
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
  clientName,
  name,
  scope,
  scopeRequest,
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
  clientName: string;
  name: string;
  scope: ResolvedSiteScope;
  scopeRequest: MintScopeRequest;
}) {
  const trimmedName = name.trim();
  const nameOk = trimmedName.length > 0;
  const blocked = mintBlockedReason(nameOk, scope, scopeRequest);
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
          // rather than sending `?? []` keeps that impossible instead of quiet.
          if (!scopeRequest.ok) return;
          // Read here, at the moment the request is built, and closed over.
          // Reading them again inside onSuccess is the whole defect: that runs
          // after a round trip the operator can type through.
          const mintedFor = { scope, clientName };
          mint.mutate(
            { name: trimmedName, ...scopeRequest.scope },
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
 *
 * THE SHELL SETUP COMMAND FOR {clientName} IS NOT RENDERED HERE. This client's
 * table entry (client-table.ts) emits a JSON or raw config, not a CLI
 * invocation, so there is no `claude mcp add`-shaped command in this codebase
 * to fill in without inventing a third snippet kind under time pressure. What
 * ships is the reveal itself, matching the wireframe's non-negotiables; the
 * setup-command shape is outstanding.
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
