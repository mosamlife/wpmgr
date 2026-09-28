import { AlertTriangle } from "lucide-react";

import type { Site } from "@wpmgr/api";

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

function describeUnreadableItems(unreadable: string[] | undefined): string {
  const labels = new Set<string>();
  for (const key of unreadable ?? []) {
    labels.add(UNREADABLE_ITEM_LABELS[key] ?? "some stored credentials");
  }
  if (labels.size === 0) return "some stored credentials";
  return formatList(Array.from(labels));
}

function formatList(items: string[]): string {
  if (items.length === 1) return items[0];
  if (items.length === 2) return `${items[0]} and ${items[1]}`;
  return `${items.slice(0, -1).join(", ")}, and ${items[items.length - 1]}`;
}

export function KeystoreStatusAlert({ site }: { site: Site }) {
  const status = site.keystore_status;
  const state = status?.state;
  if (state !== "unreadable" && state !== "key_unavailable") return null;

  const adminUrl = `${stripTrailingSlash(site.url)}/wp-admin/`;
  const backupsAffected =
    state === "key_unavailable" || status?.items?.age_identity === "unreadable";
  const saltsSuspected = status?.key_source === "salts";

  const heading = backupsAffected
    ? "Backups cannot run for this site."
    : `This site's ${describeUnreadableItems(status?.unreadable)} cannot be read.`;

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
