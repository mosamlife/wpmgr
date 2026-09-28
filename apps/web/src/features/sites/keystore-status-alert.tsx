import { AlertTriangle } from "lucide-react";

import type { Site, SiteKeystoreStatus } from "@wpmgr/api";

// KeystoreStatusAlert, GH #753 slice 1b.
//
// Site-level alert for the operator-visible half of the agent's keystore
// probe (Keystore::probe(), decoded on the CP side into `Site.keystore_status`,
// see apps/api/internal/site/service.go). Shown on the site overview
// (`$siteId.health.tsx`) and in the Backups section
// (`features/backups/backups-section.tsx`), BEFORE any backup runs, so an
// operator sees the warning whether they land on the site's overview or go
// straight to Backups.
//
// Renders for `unreadable` (the master key resolved but one or more stored
// items did not decrypt under it) and `key_unavailable` (the master key
// itself could not be resolved). Renders NOTHING for `ok`, for
// `not_reported` (the latest metadata push carried no recognised probe
// result: a pre-#753 agent that has since synced, or a later push the
// control plane didn't recognise) and for a `site` with no `keystore_status`
// at all (absent only before the site's first metadata sync). The status
// arrives with the agent's ordinary metadata push, the 30-minute cron
// cadence or a CP-triggered recheck, never on admin_init.
//
// `unreadable` does NOT always mean backups are broken: the master key can
// resolve fine while a single unrelated item (an email credential, say)
// fails to decrypt. Backups only depend on `items.age_identity`, so the
// "backups cannot run" copy is gated on that item specifically (or on
// `key_unavailable`, which blocks every item including it). Otherwise the
// alert names what's actually unreadable in plain words.
//
// A push can also carry state=unreadable with no usable `items`/`unreadable`
// detail at all (an agent that sends the state but a malformed items/
// unreadable shape the control plane couldn't parse; see the tolerant decode
// in apps/api/internal/agent/handler.go). That case cannot be scored against
// `items.age_identity`, so it must not silently fall through to the
// backups-are-fine branch: it renders its own heading, which says plainly
// that this report doesn't say which items are affected, including whether
// backups are among them.
//
// Deliberately no mechanism in the copy beyond the WordPress security-key
// (salts) cause, which is only offered when `key_source` says the master key
// is actually pinned by salts, never asserted for any other source, and no
// reset action yet: slice 2 adds an audited reset; until then the fix lives
// in the site's own WordPress admin.

const UNREADABLE_ITEM_LABELS: Record<string, string> = {
  site_keypair: "connection keys",
  cp_public_key: "connection keys",
  email_secret: "email credentials",
  email_connection_secrets: "email credentials",
};

// PR #778 item 1 (Greptile review). `status.unreadable` is a convenience list
// that is supposed to mirror the "unreadable" entries in `status.items`, but
// a push can arrive with a usable `items` map and an `unreadable` field that
// is missing or malformed (not an array, since the tolerant decode on the CP
// side means the wire shape isn't guaranteed even though the generated type
// says `Array<string>`). Preferring the list and silently giving up when it
// isn't usable threw away detail the `items` map still had.
//
// Resolves the actual set of unreadable item keys: the `unreadable` list when
// it is a non-empty array, otherwise the `items` entries whose value is
// "unreadable". When both are present and disagree, the list wins, since it
// is the field the control plane populates deliberately for this purpose
// rather than a byproduct of the per-item map.
function resolveUnreadableKeys(status: SiteKeystoreStatus | undefined): string[] {
  const list = status?.unreadable;
  if (Array.isArray(list) && list.length > 0) return list;
  const items = status?.items;
  if (!items || typeof items !== "object") return [];
  return Object.keys(items).filter((key) => items[key] === "unreadable");
}

// Returns null when there is no usable item detail to name (neither the
// `unreadable` list nor the `items` map yielded anything), so the caller can
// fall back to a heading that doesn't pretend to know what's affected.
function describeUnreadableItems(unreadable: string[]): string | null {
  if (unreadable.length === 0) return null;
  const labels = new Set<string>();
  for (const key of unreadable) {
    labels.add(UNREADABLE_ITEM_LABELS[key] ?? "other stored credentials");
  }
  return formatList(Array.from(labels));
}

function formatList(items: string[]): string {
  if (items.length === 0) return "";
  const rest = items.slice(0, -1);
  const last = items.at(-1) ?? "";
  if (rest.length === 0) return last;
  return `${rest.join(", ")}${rest.length > 1 ? "," : ""} and ${last}`;
}

export function KeystoreStatusAlert({ site }: { site: Site }) {
  const status = site.keystore_status;
  const state = status?.state;
  if (state !== "unreadable" && state !== "key_unavailable") return null;

  const adminUrl = `${stripTrailingSlash(site.url)}/wp-admin/`;
  // PR #778 item 3 (CodeRabbit). `backupsAffected` used to read
  // `status.items?.age_identity` directly, independent of the resolved set
  // above. A report whose `unreadable` list named age_identity but whose
  // `items` map was missing or omitted it then named the backup key as
  // unreadable in the heading while this flag stayed false, so the two
  // sentences contradicted each other. Both must be computed from the same
  // resolved set.
  const resolvedUnreadable = resolveUnreadableKeys(status);
  const backupsAffected =
    state === "key_unavailable" || resolvedUnreadable.includes("age_identity");
  const saltsSuspected = status?.key_source === "salts";
  const unreadableItems = describeUnreadableItems(resolvedUnreadable);

  const heading = backupsAffected
    ? "Backups cannot run for this site."
    : unreadableItems
      ? `This site's ${unreadableItems} cannot be read.`
      : "This site has stored credentials that cannot be read, and it is not known whether backups are affected.";

  return (
    <div
      role="alert"
      className="flex items-start gap-3 rounded-lg bg-[var(--color-destructive-subtle)] px-4 py-3 text-sm text-[var(--color-destructive-subtle-fg)]"
    >
      <AlertTriangle aria-hidden="true" className="mt-px size-4 shrink-0" />
      <div className="min-w-0 flex-1 space-y-1">
        <p className="font-medium">{heading}</p>
        <p>
          {saltsSuspected
            ? "This most often happens when the site moved to a different host, or its WordPress security keys changed. "
            : null}
          Open this site&apos;s WordPress admin for the exact steps to fix
          it. A reset option is coming soon.
        </p>
      </div>
      <a
        href={adminUrl}
        target="_blank"
        rel="noopener noreferrer"
        className="shrink-0 whitespace-nowrap font-medium underline underline-offset-2 hover:no-underline"
      >
        Open wp-admin
      </a>
    </div>
  );
}

function stripTrailingSlash(url: string): string {
  return url.endsWith("/") ? url.slice(0, -1) : url;
}
