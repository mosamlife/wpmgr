import { AlertTriangle } from "lucide-react";

import type { Site } from "@wpmgr/api";

// KeystoreStatusAlert — GH #753 slice 1b.
//
// Site-level alert for the operator-visible half of the agent's keystore
// probe (Keystore::probe(), decoded on the CP side into `Site.keystore_status`
// — see apps/api/internal/site/service.go). Shown on the site overview
// (`$siteId.health.tsx`) and in the Backups section
// (`features/backups/backups-section.tsx`), BEFORE any backup runs, so an
// operator sees the warning whether they land on the site's overview or go
// straight to Backups.
//
// Renders for `unreadable` (the master key resolved but a stored item did
// not decrypt under it) and `key_unavailable` (the master key itself could
// not be resolved) — both mean the same thing to an operator: backups for
// this site cannot run right now. Renders NOTHING for `ok`, for
// `not_reported` (an agent that predates GH #753, or one that has never
// pushed a probe yet) and for a `site` with no `keystore_status` at all
// (same as `not_reported` — kept explicit on the wire so a status-only
// metadata push is never confused with "ok").
//
// Deliberately no mechanism in the copy (site moved host / keys changed is
// offered as "most often", never asserted) and no reset action yet — slice 2
// adds an audited reset; until then the fix lives in the site's own
// WordPress admin.

export function KeystoreStatusAlert({ site }: { site: Site }) {
  const state = site.keystore_status?.state;
  if (state !== "unreadable" && state !== "key_unavailable") return null;

  const adminUrl = `${stripTrailingSlash(site.url)}/wp-admin/`;

  return (
    <div
      role="alert"
      className="flex items-start gap-3 rounded-lg bg-[var(--color-destructive-subtle)] px-4 py-3 text-sm text-[var(--color-destructive-subtle-fg)]"
    >
      <AlertTriangle aria-hidden="true" className="mt-px size-4 shrink-0" />
      <div className="min-w-0 flex-1 space-y-1">
        <p className="font-medium">Backups cannot run for this site.</p>
        <p>
          This most often happens when the site moved to a different host, or
          its WordPress security keys changed. Open this site&apos;s
          WordPress admin for the exact steps to fix it. A reset option is
          coming soon.
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
