import { useMemo, useState } from "react";
import { AlertTriangle, ShieldAlert } from "lucide-react";
import { z } from "zod";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { PageError } from "@/components/feedback/page-error";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

import {
  allScopesRecognised,
  describeScope,
  type ConsentContext,
  type SelfAsserted,
} from "./consent-context";
import {
  describeSiteScope,
  isScopeApprovable,
  resolveSiteScope,
  resolveTagIds,
  type FleetSnapshot,
  type ResolvedSiteScope,
  type ScopedSite,
  type SiteScopeMode,
} from "./site-scope";
import { SiteEnforcementBox } from "./site-enforcement-box";

// The consent screen (design Step 7).
//
// The design specifies this screen as written from a checklist -- which client
// is asking, what it may read, what it may propose, that it cannot approve
// anything, how many sites and which, how long the grant lasts, where to revoke
// -- and instructs that every sentence be written fresh from it, because "blunt
// beats euphemistic here".
//
// Each checklist item is marked below with the section that discharges it. The
// "propose" item is deliberately left undischarged: no capability in the
// shipped vocabulary does anything but read (the capability CHECK constraint
// admits only members ending `.read`; see migration m131), so there is no
// propose behaviour for this screen to describe.

export const REVOKE_LOCATION = "Settings, under AI connections";

// ---------------------------------------------------------------------------
// Checklist item 1: which client is asking
// ---------------------------------------------------------------------------

/**
 * The identity block.
 *
 * m124 obligation 7. This is the security control of the whole screen, so it is
 * built so that the verified fact leads and the self-declared one follows,
 * rather than the other way round which is how every OAuth consent screen in
 * the world is laid out and is exactly the habit an impersonating registration
 * relies on.
 *
 * The self-declared name is:
 *   - never the heading;
 *   - always inside quotation marks, so it reads as reported speech;
 *   - always immediately followed by the statement that we did not verify it;
 *   - never rendered as a link, even when client_uri is a URL, because a link
 *     is an endorsement and a click target we did not check.
 */
function IdentityBlock({ consent }: { consent: ConsentContext }) {
  return (
    <section
      aria-labelledby="consent-identity-heading"
      className="rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4"
    >
      <h2
        id="consent-identity-heading"
        className="text-sm font-medium text-[var(--color-muted-foreground)]"
      >
        Who is asking
      </h2>

      {/* THE VERIFIED FACT, FIRST AND LARGEST. redirect_host is derived from
          the redirect_uri the server exact-matched against the client's
          registered array. It is the only part of this request we checked. */}
      <p className="mt-2 text-xs uppercase tracking-wide text-[var(--color-muted-foreground)]">
        We verified this
      </p>
      <p
        data-testid="consent-redirect-host"
        className="mt-1 break-all font-mono text-lg font-semibold text-[var(--color-foreground)]"
      >
        {consent.redirectHost}
      </p>
      <p className="mt-1 text-sm text-[var(--color-muted-foreground)]">
        After you approve, this browser sends the connection back to{" "}
        <span className="break-all font-mono">{consent.redirectUri}</span>. We matched that
        address exactly against the one this client registered. It is the one thing on this
        screen we can stand behind.
      </p>

      {/* THE SELF-DECLARED FACT, SECOND, MARKED. */}
      <div className="mt-4 border-t border-[var(--color-border)] pt-4">
        <p className="flex items-center gap-1.5 text-xs uppercase tracking-wide text-[var(--color-muted-foreground)]">
          <ShieldAlert aria-hidden="true" className="size-3.5" />
          We did not verify this
        </p>
        <p data-testid="consent-client-name" className="mt-1 text-sm text-[var(--color-foreground)]">
          <SelfAssertedName value={consent.clientNameUnverified} />
        </p>
        <p className="mt-1 text-sm text-[var(--color-muted-foreground)]">
          Anyone can register a client under any name, including the name of software you
          trust. We have no way to tell a real one from a copy. Judge this request by the
          address above, not by the name.
        </p>
        <SelfAssertedSite value={consent.clientUriUnverified} />
        <p className="mt-2 text-xs text-[var(--color-muted-foreground)]">
          Client ID <span className="break-all font-mono">{consent.clientId}</span>
        </p>
      </div>
    </section>
  );
}

/**
 * A self-declared name, or the statement that there was not one.
 *
 * An absent name renders as a sentence, not as a blank. A blank is the house
 * defect class -- an absence coerced into a plausible value -- and here the
 * plausible value is "a client whose name simply did not fit on screen", which
 * is a strictly worse thing for the user to believe than the truth.
 */
function SelfAssertedName({ value }: { value: SelfAsserted }) {
  if (!value.stated) {
    return (
      <span data-testid="consent-client-name-absent" className="italic">
        This client did not give a name.
      </span>
    );
  }
  return (
    <>
      It calls itself <span className="font-medium">&ldquo;{value.value}&rdquo;</span>.
    </>
  );
}

/** The self-declared homepage, shown as text and never as a link. */
function SelfAssertedSite({ value }: { value: SelfAsserted }) {
  if (!value.stated) return null;
  return (
    <p className="mt-2 text-sm text-[var(--color-muted-foreground)]">
      It gives its homepage as{" "}
      <span data-testid="consent-client-uri" className="break-all font-mono">
        {value.value}
      </span>
      . Not shown as a link, because we did not check where it goes.
    </p>
  );
}

// ---------------------------------------------------------------------------
// Checklist items 2, 3 and 4: what it may read, what it may propose, and that
// it cannot approve anything
//
// The "may propose" third of that checklist item has no section below: no
// capability in the shipped vocabulary does anything but read (the capability
// CHECK constraint admits only members ending `.read`; see migration m131),
// so there is nothing to disclose here and adding placeholder copy for it
// would be a false capability claim.
// ---------------------------------------------------------------------------

function PermissionsBlock({ consent }: { consent: ConsentContext }) {
  const recognised = allScopesRecognised(consent.scopes);
  return (
    <section
      aria-labelledby="consent-permissions-heading"
      className="rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4"
    >
      <h2 id="consent-permissions-heading" className="text-sm font-medium">
        What this connection can do
      </h2>

      <ul className="mt-3 space-y-3">
        {consent.scopes.map((token) => {
          const copy = describeScope(token);
          return (
            <li key={token}>
              <p className="text-sm font-medium">{copy.title}</p>
              <p className="mt-0.5 text-sm text-[var(--color-muted-foreground)]">{copy.detail}</p>
            </li>
          );
        })}
      </ul>

      {/* THE NEGATIVE HALF, GIVEN THE SAME WEIGHT AS THE POSITIVE HALF.
          A user who believes an AI has write access declines something safe; a
          user who believes the reverse approves something they should not. Both
          errors are caused by a screen that only enumerates what is granted. */}
      <div className="mt-4 border-t border-[var(--color-border)] pt-4">
        <h3 className="text-sm font-medium">What it cannot do</h3>
        <ul className="mt-2 space-y-2 text-sm text-[var(--color-muted-foreground)]">
          <li>
            <span className="font-medium text-[var(--color-foreground)]">
              It cannot change anything.
            </span>{" "}
            No updates, no installs, no activations, no deletions, no edits to any site, and no
            changes to this dashboard or your organisation. This connection is read-only.
          </li>
          <li>
            <span className="font-medium text-[var(--color-foreground)]">
              It cannot approve anything.
            </span>{" "}
            Everything that changes a site is done by a person, here in this dashboard. There is
            no setting or mode that lets this connection do it instead.
          </li>
          <li>
            <span className="font-medium text-[var(--color-foreground)]">
              It cannot reach your sites directly.
            </span>{" "}
            It talks to this dashboard. It gets no WordPress logins, no admin access, no
            database access and no file access.
          </li>
          <li>
            <span className="font-medium text-[var(--color-foreground)]">
              It cannot read your backups&apos; contents.
            </span>{" "}
            It can see that a backup exists and whether it succeeded. It cannot download one or
            read what is inside it.
          </li>
        </ul>
      </div>

      {!recognised && (
        <p
          role="alert"
          data-testid="consent-unrecognised-scope"
          className="mt-4 flex items-start gap-2 rounded-md border border-[var(--color-destructive)]/30 p-3 text-sm text-[var(--color-destructive)]"
        >
          <AlertTriangle aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
          This client asked for a permission this dashboard does not recognise, so we cannot
          tell you what approving it would allow. Do not approve it.
        </p>
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Checklist item 5: how many sites, and which
// ---------------------------------------------------------------------------

function SiteScopeBlock({
  mode,
  onModeChange,
  scope,
  tags,
  selectedTagNames,
  onToggleTag,
  sites,
  pickerComplete,
  selectedSiteIds,
  onToggleSite,
}: {
  mode: SiteScopeMode;
  onModeChange: (mode: SiteScopeMode) => void;
  scope: ResolvedSiteScope;
  tags: readonly { id: string; name: string }[] | null;
  selectedTagNames: readonly string[];
  onToggleTag: (name: string) => void;
  sites: readonly ScopedSite[] | null;
  pickerComplete: boolean;
  selectedSiteIds: readonly string[];
  onToggleSite: (id: string) => void;
}) {
  // A COUNT ALONE IS NOT ENOUGH when the scope is a list or a tag, so the
  // resolved sites are always enumerated rather than summarised. The union's
  // `none` and `unresolved` members have no site list to enumerate and say so
  // instead, in words that cannot be read as "everything".
  const enumerable =
    scope.kind === "all" ? scope.shown : scope.kind === "sites" ? scope.sites : [];

  // Whether the enumeration below is the whole story. Rendered explicitly
  // rather than inferred from the list's length, because "these are all of
  // them" and "these are the ones we could load" look identical on screen and
  // mean very different things to someone about to approve.
  const listComplete =
    scope.kind === "all" || scope.kind === "sites" ? scope.listComplete : true;

  return (
    <section
      aria-labelledby="consent-sites-heading"
      className="rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4"
    >
      <h2 id="consent-sites-heading" className="text-sm font-medium">
        Which sites it can read
      </h2>

      <fieldset className="mt-3">
        <legend className="sr-only">Site scope</legend>
        <div className="flex flex-wrap gap-2">
          {(
            [
              ["all", "Every site"],
              ["tags", "Sites with a tag"],
              ["list", "Sites I pick"],
            ] as const
          ).map(([value, label]) => (
            <label
              key={value}
              className={cn(
                "cursor-pointer rounded-md border px-3 py-1.5 text-sm",
                // The ring rides on the label because the input it belongs to
                // is visually hidden; without this a keyboard user cannot see
                // which option they are on.
                "has-[:focus-visible]:outline-none has-[:focus-visible]:ring-2",
                "has-[:focus-visible]:ring-[var(--color-ring)] has-[:focus-visible]:ring-offset-2",
                "has-[:focus-visible]:ring-offset-[var(--color-card)]",
                mode === value
                  ? "border-[var(--color-primary)] bg-[var(--color-primary)]/10 font-medium"
                  : "border-[var(--color-border)]",
              )}
            >
              <input
                type="radio"
                name="site-scope-mode"
                className="sr-only"
                value={value}
                checked={mode === value}
                onChange={() => onModeChange(value)}
              />
              {label}
            </label>
          ))}
        </div>
      </fieldset>

      {mode === "tags" && (
        <div className="mt-3 flex flex-wrap gap-3">
          {/* NULL IS NOT EMPTY. A failed tags request used to arrive here as an
              empty array, and the screen then stated a fact about the
              organisation that it did not know -- the same collapse of "we
              could not ask" into "there are none" that the site list carries
              a FleetSnapshot to avoid. Two queries, one screen, one consent
              decision; they have to be honest in the same way. */}
          {tags === null ? (
            <p
              role="alert"
              data-testid="consent-tags-failed"
              className="text-sm text-[var(--color-destructive)]"
            >
              We could not load this organisation&apos;s tags, so we cannot show you what
              this would cover. That is not the same as having no tags. Do not approve
              until this loads.
            </p>
          ) : tags.length === 0 ? (
            <p data-testid="consent-tags-empty" className="text-sm text-[var(--color-muted-foreground)]">
              This organisation has no tags yet, so there is nothing to scope by.
            </p>
          ) : (
            tags.map((tag) => (
              <label key={tag.id} className="flex items-center gap-2 text-sm">
                <Checkbox
                  checked={selectedTagNames.includes(tag.name)}
                  onChange={() => onToggleTag(tag.name)}
                />
                {tag.name}
              </label>
            ))
          )}
        </div>
      )}

      {mode === "list" && sites === null && (
        <p
          role="alert"
          data-testid="consent-sites-failed"
          className="mt-3 rounded-md border border-[var(--color-destructive)]/30 p-3 text-sm text-[var(--color-destructive)]"
        >
          We could not load this organisation&apos;s sites, so there is nothing to pick
          from. That is not the same as having no sites. Do not approve until this loads.
        </p>
      )}

      {mode === "list" && sites !== null && !pickerComplete && (
        <p
          role="alert"
          data-testid="consent-picker-truncated"
          className="mt-3 rounded-md border border-[var(--color-destructive)]/30 p-3 text-sm text-[var(--color-destructive)]"
        >
          This organisation has more sites than we can list here, so the choices below
          are not all of them. Anything you do not see is not covered by this grant.
        </p>
      )}

      {mode === "list" && sites !== null && (
        <div className="mt-3 max-h-56 space-y-2 overflow-y-auto">
          {sites.map((site) => (
            <label key={site.id} className="flex items-start gap-2 text-sm">
              <Checkbox
                checked={selectedSiteIds.includes(site.id)}
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

      <p
        data-testid="consent-scope-summary"
        className={cn(
          "mt-4 text-sm",
          isScopeApprovable(scope)
            ? "text-[var(--color-foreground)]"
            : "text-[var(--color-destructive)]",
        )}
      >
        {describeSiteScope(scope)}
      </p>

      {enumerable.length > 0 && !listComplete && (
        <p
          data-testid="consent-list-partial"
          className="mt-2 text-sm font-medium text-[var(--color-destructive)]"
        >
          The sites below are the ones we could load, not the whole list.
        </p>
      )}

      {enumerable.length > 0 && (
        <ul
          data-testid="consent-scope-sites"
          className="mt-2 max-h-40 space-y-1 overflow-y-auto text-sm text-[var(--color-muted-foreground)]"
        >
          {enumerable.map((site) => (
            <li key={site.id} className="break-all">
              {site.name} <span className="font-mono text-xs">{site.url}</span>
            </li>
          ))}
        </ul>
      )}

      <p className="mt-3 text-xs text-[var(--color-muted-foreground)]">
        Your organisation&apos;s records decide what this connection reads on each
        request, not this list. The list is what this dashboard could load just now.
      </p>

      {/* Screen 8 (wireframes.html#s8) — the enforcement box. No `refusals`
          prop: this connection does not exist yet (approval has not
          happened), so there is no refusal history, tracked or otherwise, to
          have an opinion about. See site-enforcement-box.tsx's module doc. */}
      <SiteEnforcementBox scope={scope} />
    </section>
  );
}

// ---------------------------------------------------------------------------
// Checklist items 6 and 7: how long the grant lasts, and where to revoke
// ---------------------------------------------------------------------------

/**
 * Duration and revocation.
 *
 * A TERM IN DAYS, FROM THE SERVER, AND NOT A DATE THIS FILE COMPUTES.
 * `grantLifetimeDays` is `grantAbsoluteTTL` (apps/api/internal/mcp/service.go)
 * carried on the consent payload. Every grant is stamped with
 * `mcp_grants.expires_at` at approval, the column is NOT NULL with no default
 * (m127 decision 2), and the authentication lookup gates on
 * `g.expires_at > now()` (apps/api/db/query/mcp_connections.sql:561), so
 * expiry is a fact about the connection and not a background intention.
 *
 * Two things are deliberately absent. The first is a calendar date: no grant
 * exists while this screen is open, the stamp happens at approval, and a date
 * rendered here would be earlier than the one the row actually gets. The
 * second is idle expiry: `mcp_grants.idle_expire_after_days` exists but
 * CreateGrant writes NULL and NULL means never idle-expire, so no unused
 * connection is closed for being unused today. Describing a control that is
 * not running would be its own untrue sentence.
 *
 * Until 2026-08-31 this block said the connection "does not expire on its own",
 * over a comment asserting mcp_grants had no expires_at column. The column
 * landed in m127 and the sentence did not follow it, so the screen spent that
 * window telling people a 90 day authorisation was open-ended at the moment
 * they decided to grant it. The regression test asserts the replacement by
 * value.
 */
function DurationBlock({ lifetimeDays }: { readonly lifetimeDays: number }) {
  return (
    <section
      aria-labelledby="consent-duration-heading"
      className="rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4"
    >
      <h2 id="consent-duration-heading" className="text-sm font-medium">
        How long this lasts, and how to stop it
      </h2>
      <p
        data-testid="consent-duration-expiry"
        className="mt-2 text-sm text-[var(--color-muted-foreground)]"
      >
        This connection expires on its own {lifetimeDays} days after you approve it. Once it
        expires the client is refused at its next request and reads nothing further. Getting it
        working again means coming back to this screen and approving a new connection.
      </p>
      <p className="mt-2 text-sm text-[var(--color-muted-foreground)]">
        You can end it sooner in {REVOKE_LOCATION}, and that takes effect immediately. Every
        request this connection makes is checked against that setting, so revoking stops it at
        its next request rather than waiting for the {lifetimeDays} days to run out.
      </p>
    </section>
  );
}

// ---------------------------------------------------------------------------
// The screen
// ---------------------------------------------------------------------------

/**
 * Unwrap a tag payload that the approve gate has already proved is present.
 *
 * A throw rather than a default. The whole finding was a `?? []` standing where
 * a refusal belonged, and replacing it with a different silent fallback would
 * leave the same shape in place for the next refactor to trip over.
 */
function assertTagPayload(payload: readonly string[] | null): string[] {
  if (payload === null) {
    throw new Error("consent: refused to submit a tag scope with no tags");
  }
  return [...payload];
}

export const consentNameSchema = z.object({
  name: z.string().trim().min(1, "Give this connection a name so you can find it later."),
});

export interface ConsentScreenProps {
  readonly consent: ConsentContext;
  /**
   * The tenant's tag registry, or null when we could not load it. Null is NOT
   * an empty registry: one is a fact about the organisation and the other is a
   * fact about our request, and only one of them belongs on a consent screen.
   */
  readonly tags: readonly { id: string; name: string }[] | null;
  /**
   * What we loaded of the fleet, or null when the load has not finished or
   * failed. Null is NOT an empty snapshot, and an incomplete snapshot is NOT a
   * complete one -- see site-scope.ts.
   */
  readonly fleet: FleetSnapshot | null;
  readonly tagsBySiteId: Readonly<Record<string, readonly string[]>>;
  readonly sitesLoading: boolean;
  readonly isApproving: boolean;
  readonly approveError: Error | null;
  readonly onApprove: (input: {
    name: string;
    siteScopeMode: SiteScopeMode;
    scopeTagIds: string[];
    scopeSiteIds: string[];
  }) => void;
  readonly onDeny: () => void;
}

export function ConsentScreen({
  consent,
  tags,
  fleet,
  tagsBySiteId,
  sitesLoading,
  isApproving,
  approveError,
  onApprove,
  onDeny,
}: ConsentScreenProps) {
  // No default mode. 'all' is not a starting position on a screen whose whole
  // job is to make the operator choose; the schema refuses an unset mode for
  // the same reason (m124 DECISION 1, "there is deliberately no DEFAULT 'all'").
  const [mode, setMode] = useState<SiteScopeMode>("list");
  const [selectedTagNames, setSelectedTagNames] = useState<readonly string[]>([]);
  const [selectedSiteIds, setSelectedSiteIds] = useState<readonly string[]>([]);
  const [name, setName] = useState(
    consent.clientNameUnverified.stated ? consent.clientNameUnverified.value : "",
  );
  const [nameError, setNameError] = useState<string | null>(null);

  const scope = useMemo(
    () =>
      resolveSiteScope({
        mode,
        selectedTagNames,
        selectedSiteIds,
        fleet,
        tagsBySiteId,
        sitesLoading,
      }),
    [mode, selectedTagNames, selectedSiteIds, fleet, tagsBySiteId, sitesLoading],
  );

  const scopeOk = isScopeApprovable(scope);
  const scopesOk = allScopesRecognised(consent.scopes);

  // THE PAYLOAD THE SUBMIT PATH WOULD ACTUALLY SEND, resolved once and shared
  // with the approve gate, so the button and the request cannot disagree.
  //
  // resolveSiteScope reads `fleet` and `tagsBySiteId`; it never reads `tags`.
  // So a registry that goes null AFTER a tag is ticked leaves the scope
  // resolving happily to a real site set while the id lookup behind it has
  // nothing left to look in. `(tags ?? []).filter(...)` then yields [], and the
  // operator's chosen tag is dropped from the request they are pressing the
  // button on. Null refused to guess in the display path and still resolved to
  // an empty array one function below it.
  //
  // Null here means "cannot build this payload", and empty means the same
  // thing: a selected tag that no longer resolves to an id (deleted, or a
  // registry reloaded without it) is an absence, not a request for nothing.
  // m124's CHECK and internal/mcp/scope.go:177-182 both refuse mode 'tags'
  // with an empty array, so this is the client half of a gate the server
  // already holds -- and the half that keeps the button honest.
  const tagPayload = useMemo((): readonly string[] | null => {
    if (mode !== "tags") return [];
    return resolveTagIds(selectedTagNames, tags);
  }, [mode, tags, selectedTagNames]);

  const canApprove = scopeOk && scopesOk && tagPayload !== null && !isApproving;

  function handleSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = consentNameSchema.safeParse({ name });
    if (!parsed.success) {
      setNameError(parsed.error.issues[0]?.message ?? "Give this connection a name.");
      return;
    }
    setNameError(null);
    if (!canApprove) return;
    onApprove({
      name: parsed.data.name,
      siteScopeMode: mode,
      // The payload each mode is allowed to carry, and only that payload.
      // mcp_grants_site_scope_payload_check refuses any other combination, so
      // sending the unused array populated would be a 400 the user cannot act on.
      // Never `?? []`. canApprove is false whenever tagPayload is null, so this
      // is unreachable with a null payload; it throws rather than defaulting so
      // that stays true if the gate is ever refactored apart from it.
      scopeTagIds: assertTagPayload(tagPayload),
      scopeSiteIds: mode === "list" ? [...selectedSiteIds] : [],
    });
  }

  return (
    <form onSubmit={handleSubmit} noValidate className="mx-auto max-w-2xl space-y-4 p-4 sm:p-6">
      <header>
        <h1 className="text-xl font-semibold">Approve an AI connection</h1>
        <p className="mt-1 text-sm text-[var(--color-muted-foreground)]">
          Something is asking to read your fleet through this dashboard. Read this before you
          approve it.
        </p>
      </header>

      <IdentityBlock consent={consent} />
      <PermissionsBlock consent={consent} />
      <SiteScopeBlock
        mode={mode}
        onModeChange={setMode}
        scope={scope}
        tags={tags}
        selectedTagNames={selectedTagNames}
        onToggleTag={(tagName) =>
          setSelectedTagNames((prev) =>
            prev.includes(tagName) ? prev.filter((t) => t !== tagName) : [...prev, tagName],
          )
        }
        sites={fleet === null ? null : fleet.sites}
        pickerComplete={fleet === null ? true : fleet.complete}
        selectedSiteIds={selectedSiteIds}
        onToggleSite={(id) =>
          setSelectedSiteIds((prev) =>
            prev.includes(id) ? prev.filter((s) => s !== id) : [...prev, id],
          )
        }
      />
      <DurationBlock lifetimeDays={consent.grantLifetimeDays} />

      <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4">
        <Label htmlFor="consent-name">Name this connection</Label>
        <Input
          id="consent-name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          aria-invalid={nameError !== null}
          aria-describedby={nameError !== null ? "consent-name-error" : undefined}
          className="mt-1.5"
        />
        {nameError !== null && (
          <p id="consent-name-error" role="alert" className="mt-1.5 text-sm text-[var(--color-destructive)]">
            {nameError}
          </p>
        )}
        <p className="mt-1.5 text-xs text-[var(--color-muted-foreground)]">
          Only you see this. The name above it is the client&apos;s own and we did not check it.
        </p>
      </div>

      {approveError !== null && (
        <PageError
          what="We could not approve this connection."
          why={approveError.message}
        />
      )}

      <div className="flex flex-wrap items-center gap-3">
        <Button type="submit" disabled={!canApprove} data-testid="consent-approve">
          {isApproving ? "Approving…" : "Approve and connect"}
        </Button>
        <Button type="button" variant="outline" onClick={onDeny} data-testid="consent-deny">
          Do not connect
        </Button>
      </div>
    </form>
  );
}

/** The skeleton state. Deliberately has no approve control. */
export function ConsentScreenSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading the connection request"
      className="mx-auto max-w-2xl space-y-4 p-4 sm:p-6"
    >
      <Skeleton className="h-7 w-64" />
      <Skeleton className="h-40 w-full" />
      <Skeleton className="h-56 w-full" />
      <Skeleton className="h-40 w-full" />
    </div>
  );
}
