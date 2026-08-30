import { useMemo, useState, type ReactNode } from "react";
import { AlertTriangle, Check, ExternalLink, Lock } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { CopyableMono } from "@/components/shared/copyable-mono";
import { cn } from "@/lib/utils";

import {
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
  type MintedConnection,
} from "./use-ai-connections";
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
// Step 4 (capabilities) and the token artefact are NOT stubbed here. Their
// schema and their endpoint do not exist yet, and a disabled control for a
// feature with no backend is a promise this file cannot keep.
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
              title="Sign in through the browser"
              body="You approve the connection here, and the client stores its own key. Nothing to copy or keep secret."
              availability={client.auth.oauth}
              selected={method === "oauth"}
              onSelect={() => setMethod("oauth")}
            />
            <AuthCard
              method="token"
              title="Connection token"
              body="The documented path for CI, containers and SSH sessions, where no browser can open."
              availability={client.auth.token}
              selected={method === "token"}
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
                clientName={client.name}
                name={name}
                scope={scope}
                siteScopeMode={selection.mode}
                scopeTagIds={scopeTagIds}
                scopeSiteIds={selection.siteIds}
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
        {/* Capabilities are NOT a numbered step in this rail. The grant columns
            behind them do not exist yet, and a number for a screen that cannot
            be built is a promise the rail has no way to keep. What it names
            instead is where that decision is made today. */}
        <span>Permissions are chosen on the approval screen</span>
      </li>
    </ol>
  );
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
  onSelect,
}: {
  client: McpClientRow;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      aria-pressed={selected}
      className={cn(
        "flex h-full w-full flex-col items-start gap-1 rounded-lg border p-3 text-left transition-colors",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]",
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
  availability,
  selected,
  onSelect,
}: {
  method: AuthMethod;
  title: string;
  body: string;
  availability: AuthAvailability;
  selected: boolean;
  onSelect: () => void;
}) {
  const disabled = availability.state !== "available";
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

/**
 * The reason minting is refused, or null when it is not.
 *
 * Every branch here is a FAILURE, rendered with a remedy, never a silently
 * disabled button. `tagsResolved` being false is the client-side half of the
 * refusal this whole panel exists to make loud: see resolveTagIds.
 */
function mintBlockedReason(
  nameOk: boolean,
  scope: ResolvedSiteScope,
  tagsResolved: boolean,
): string | null {
  if (!nameOk) return "Name this connection before minting a token.";
  if (!tagsResolved) {
    return "A tag you picked in step 3 no longer resolves to an id -- the tag list may have reloaded since you chose it. Re-open step 3, re-pick it, and try again.";
  }
  if (!isScopeApprovable(scope)) {
    return scope.kind === "unresolved" && scope.because === "loading"
      ? "Still reading this organisation's sites for step 3. Wait for that to finish before minting."
      : "This organisation's sites could not be read for step 3, so we cannot tell what this connection would cover. Fix that above before minting.";
  }
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
 */
function TokenMintPanel({
  clientName,
  name,
  scope,
  siteScopeMode,
  scopeTagIds,
  scopeSiteIds,
}: {
  clientName: string;
  name: string;
  scope: ResolvedSiteScope;
  siteScopeMode: SiteScopeSelection["mode"];
  scopeTagIds: readonly string[] | null;
  scopeSiteIds: readonly string[];
}) {
  const mint = useMintConnection();
  const [token, setToken] = useState<MintedConnection | null>(null);
  const trimmedName = name.trim();

  const configKey = `${siteScopeMode}|${scopeSiteIds.join(",")}|${(scopeTagIds ?? []).join(",")}|${trimmedName}`;
  // React's own "adjusting state when a prop changes" pattern -- state, not a
  // ref, so this stays a value the compiler can reason about. Comparing and
  // resetting DURING RENDER (React bails out and re-renders before committing)
  // is what keeps a token or an error from ever painting even once beside a
  // configuration it was not produced for; a useEffect here would let that
  // one stale paint through first.
  const [lastConfigKey, setLastConfigKey] = useState(configKey);
  if (lastConfigKey !== configKey) {
    setLastConfigKey(configKey);
    if (token !== null) setToken(null);
    if (mint.isError || mint.isSuccess) mint.reset();
  }

  const nameOk = trimmedName.length > 0;
  const tagsResolved = scopeTagIds !== null;
  const blocked = mintBlockedReason(nameOk, scope, tagsResolved);
  const canMint = blocked === null && !mint.isPending;

  if (token !== null) {
    return <TokenReveal clientName={clientName} scope={scope} token={token} />;
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
          mint.mutate(
            {
              name: trimmedName,
              siteScopeMode,
              scopeTagIds: scopeTagIds ?? [],
              scopeSiteIds,
            },
            { onSuccess: (data) => setToken(data) },
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
function TokenReveal({
  clientName,
  scope,
  token,
}: {
  clientName: string;
  scope: ResolvedSiteScope;
  token: MintedConnection;
}) {
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
          <CopyableMono value={token.token} label="Copy the connection token" truncate />
        </div>
        <p className="mt-2 text-xs text-[var(--color-muted-foreground)]">
          {describeSiteScope(scope)} Capabilities: {capabilities}. Revocable from the connections
          list, immediately, at the next request.
        </p>
      </div>
    </div>
  );
}
