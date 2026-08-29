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
  type ResolvedSiteScope,
  type ScopedSite,
  type SiteScopeMode,
} from "./site-scope";

// The consent screen (design Step 7).
//
// The design specifies this screen as written from a checklist -- which client
// is asking, what it may read, what it may propose, that it cannot approve
// anything, how many sites and which, how long the grant lasts, where to revoke
// -- and instructs that every sentence be written fresh from it, because "blunt
// beats euphemistic here".
//
// Each checklist item is marked below with the section that discharges it.

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
            It can suggest work to you and nothing more. Anything that changes a site is
            approved by a person, in this dashboard, on a screen this connection cannot reach.
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
  selectedSiteIds,
  onToggleSite,
}: {
  mode: SiteScopeMode;
  onModeChange: (mode: SiteScopeMode) => void;
  scope: ResolvedSiteScope;
  tags: readonly { id: string; name: string }[];
  selectedTagNames: readonly string[];
  onToggleTag: (name: string) => void;
  sites: readonly ScopedSite[];
  selectedSiteIds: readonly string[];
  onToggleSite: (id: string) => void;
}) {
  // A COUNT ALONE IS NOT ENOUGH when the scope is a list or a tag, so the
  // resolved sites are always enumerated rather than summarised. The union's
  // `none` and `unresolved` members have no site list to enumerate and say so
  // instead, in words that cannot be read as "everything".
  const enumerable = scope.kind === "all" || scope.kind === "sites" ? scope.sites : [];

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
          {tags.length === 0 ? (
            <p className="text-sm text-[var(--color-muted-foreground)]">
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

      {mode === "list" && (
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
        This list is what this dashboard can see right now. Your organisation&apos;s records
        decide what the connection actually reads on each request.
      </p>
    </section>
  );
}

// ---------------------------------------------------------------------------
// Checklist items 6 and 7: how long the grant lasts, and where to revoke
// ---------------------------------------------------------------------------

/**
 * Duration and revocation.
 *
 * NO DATE IS SHOWN, because there is no expiry to show. mcp_grants (m124, lines
 * 608-648) has a `status` of 'active' or 'revoked' and NO expires_at column,
 * and consentResponseDTO carries no lifetime field. Rendering a date here would
 * be inventing one, and rendering the word "never" as a lifetime would be
 * dressing an absent field as a decision. The truthful statement is the one
 * below: it lasts until it is revoked.
 */
function DurationBlock() {
  return (
    <section
      aria-labelledby="consent-duration-heading"
      className="rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4"
    >
      <h2 id="consent-duration-heading" className="text-sm font-medium">
        How long this lasts, and how to stop it
      </h2>
      <p className="mt-2 text-sm text-[var(--color-muted-foreground)]">
        This connection does not expire on its own. It lasts until you revoke it. The key the
        client holds is short-lived and renews itself, so the connection keeps working until
        you end it.
      </p>
      <p className="mt-2 text-sm text-[var(--color-muted-foreground)]">
        You end it in {REVOKE_LOCATION}. Every request this connection makes is checked against
        that setting, so revoking stops it at its next request rather than whenever its current
        key would have expired.
      </p>
    </section>
  );
}

// ---------------------------------------------------------------------------
// The screen
// ---------------------------------------------------------------------------

export const consentNameSchema = z.object({
  name: z.string().trim().min(1, "Give this connection a name so you can find it later."),
});

export interface ConsentScreenProps {
  readonly consent: ConsentContext;
  readonly tags: readonly { id: string; name: string }[];
  /** Null when the site list has not loaded or its load failed. Never []. */
  readonly allSites: readonly ScopedSite[] | null;
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
  allSites,
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
        allSites,
        tagsBySiteId,
        sitesLoading,
      }),
    [mode, selectedTagNames, selectedSiteIds, allSites, tagsBySiteId, sitesLoading],
  );

  const scopeOk = isScopeApprovable(scope);
  const scopesOk = allScopesRecognised(consent.scopes);
  const canApprove = scopeOk && scopesOk && !isApproving;

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
      scopeTagIds:
        mode === "tags"
          ? tags.filter((t) => selectedTagNames.includes(t.name)).map((t) => t.id)
          : [],
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
        sites={allSites ?? []}
        selectedSiteIds={selectedSiteIds}
        onToggleSite={(id) =>
          setSelectedSiteIds((prev) =>
            prev.includes(id) ? prev.filter((s) => s !== id) : [...prev, id],
          )
        }
      />
      <DurationBlock />

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
    <div className="mx-auto max-w-2xl space-y-4 p-4 sm:p-6" aria-busy="true">
      <Skeleton className="h-7 w-64" />
      <Skeleton className="h-40 w-full" />
      <Skeleton className="h-56 w-full" />
      <Skeleton className="h-40 w-full" />
    </div>
  );
}
