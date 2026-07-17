import type { Metadata } from "next";
import Link from "next/link";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { Container, Section } from "@/components/ui/primitives";
import { SITE_CONFIG } from "@/lib/site";

export const revalidate = 3600;

export const metadata: Metadata = buildMetadata({
  title: "Changelog: WPMgr Release Notes",
  description:
    "Every WPMgr release, newest first. See what shipped, when, and which features were added, changed, or fixed.",
  canonical: "/changelog/",
});

// ---------------------------------------------------------------------------
// Curated release entries (newest first, harvested from CHANGELOG.md).
// We show the ~20 most recent meaningful releases. The full history is
// available on GitHub Releases.
// ---------------------------------------------------------------------------

type ChangeTag = "Added" | "Changed" | "Fixed" | "Security";

type ChangeEntry = {
  version: string;
  date: string;
  summary: string;
  items: Array<{ tag: ChangeTag; text: string }>;
  featureLinks?: Array<{ label: string; href: string }>;
};

const TAG_COLOR: Record<ChangeTag, string> = {
  Added: "var(--success, oklch(55% 0.15 145))",
  Changed: "var(--info, oklch(55% 0.12 235))",
  Fixed: "var(--warning, oklch(65% 0.15 75))",
  Security: "var(--destructive)",
};

const RELEASES: ChangeEntry[] = [
  {
    version: "0.61.69",
    date: "2026-07-17",
    summary: "Site tags: organize, filter, and bulk-manage your fleet with colored tags.",
    items: [
      {
        tag: "Added",
        text: "Sites can now be organized with tags. Create tags on the fly from a keyboard-first picker (type to search, press Enter to create), assign multiple tags per site from the site card, table row, or site settings, and every tag gets a consistent color automatically, with an optional custom color per tag.",
      },
      {
        tag: "Added",
        text: "Filter the Sites list by one or more tags with match-any or match-all semantics. Filters live in the URL so a filtered view can be bookmarked or shared, and clicking any tag chip jumps straight to that tag's sites.",
      },
      {
        tag: "Added",
        text: "Bulk tagging: select multiple sites and add or remove tags across all of them in one action, with a clear indicator when only some of the selected sites carry a tag.",
      },
      {
        tag: "Added",
        text: "A tag management page under Settings: rename a tag everywhere at once, merge duplicates, change colors, and delete with a usage count shown before anything is removed. Existing site tags are registered automatically on upgrade.",
      },
      {
        tag: "Fixed",
        text: "The Sites list stays fast even after long idle periods: uptime data shown on the list is now read from a compact per-site rollup instead of scanning the full probe history, with uptime percentages unchanged and exact.",
      },
    ],
  },
  {
    version: "0.61.40",
    date: "2026-07-08",
    summary: "Organizations can now be deleted, with a safety grace window.",
    items: [
      {
        tag: "Added",
        text: "Organization owners can delete an organization from Settings, Organization, with a type-the-name confirmation. An empty organization is removed immediately; a populated one is scheduled with a grace window: hidden right away and recoverable until the window passes, then permanently removed along with its sites' agent connections and stored data.",
      },
      {
        tag: "Fixed",
        text: "Switching the active organization now reconnects live dashboard updates to the newly active one immediately, instead of requiring a full page reload.",
      },
      {
        tag: "Fixed",
        text: "The deletion flow no longer shows a misleading \"recoverable during the grace window\" message for an empty organization, which is removed immediately and has no grace window; that message now appears only when a deletion is genuinely recoverable.",
      },
    ],
  },
  {
    version: "0.61.39",
    date: "2026-07-08",
    summary: "Real User Monitoring data collection fixed, on self-host and under any cache.",
    items: [
      {
        tag: "Fixed",
        text: "Real User Monitoring collected no data at all on self-hosted installs: the bundled reverse-proxy configuration had no route for the RUM endpoint, so every beacon request was rejected before it reached the application (the same gap silently dropped inbound email and billing webhooks too). Both are now routed correctly, and a new check exercises the real proxy configuration against every public endpoint so a gap like this is caught before release.",
      },
      {
        tag: "Fixed",
        text: "Real User Monitoring also collected no data on sites served by a third-party page cache, because the collector script was only injected inside WPMgr's own cache output. It is now injected on a standard WordPress hook during page generation, so whichever cache serves the page still captures the data.",
      },
      {
        tag: "Fixed",
        text: "The RUM beacon key could get permanently stuck: if the one-time delivery of the key to a site was ever lost, the site kept showing RUM as enabled while silently collecting nothing. The control plane now tracks whether the site has actually confirmed it holds a key and automatically reissues one when it is missing. A manual \"Rotate beacon key\" action was added, and the dashboard now warns when RUM is on but unconfirmed.",
      },
    ],
    featureLinks: [{ label: "Real User Monitoring", href: "/features/real-user-monitoring/" }],
  },
  {
    version: "0.61.37",
    date: "2026-07-07",
    summary: "Fixed the hide-login security feature: the secret URL now actually serves a login form.",
    items: [
      {
        tag: "Fixed",
        text: "Turning on \"hide login\" showed a harmless but alarming \"policy stored but agent push failed\" error even though it saved and applied correctly. More seriously, the secret login URL did not show a login form at all (it bounced to the home page), which could leave you unable to sign in through the browser. The secret URL now serves the login form correctly while the default wp-login.php stays hidden. Review also closed two smaller gaps: the secret URL is no longer exposed in page links to logged-out visitors, and the access cookie is now signed so it cannot be forged.",
      },
    ],
    featureLinks: [{ label: "Security suite", href: "/features/security/" }],
  },
  {
    version: "0.61.36",
    date: "2026-07-07",
    summary: "Fixed a bug that could permanently break an incremental backup chain.",
    items: [
      {
        tag: "Fixed",
        text: "An incremental backup could fail with a \"stalled\" error because retention cleanup had deleted a parent snapshot's file-list data while it was still needed, permanently breaking the chain. Retention now always protects the completed snapshot at each chain position and, as a ground-truth safety net, never deletes data that any surviving snapshot still references. A broken chain now fails fast with a clear \"run a full backup\" message instead of stalling silently.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
  },
  {
    version: "0.61.35",
    date: "2026-07-07",
    summary: "Vulnerability and performance tiles on the Health tab now show live data.",
    items: [
      {
        tag: "Fixed",
        text: "The Vulnerabilities and Performance tiles on a site's Health tab were placeholders that always read \"Not scanned yet\" and \"Not measured yet\", regardless of the real data. They now show live results: open-vulnerability count and worst severity, and Core Web Vitals (LCP) from real visitor data, each linking through to the full view. A site that has not been scanned, or has no visitor data yet, shows an honest empty state instead of a fabricated number.",
      },
    ],
    featureLinks: [
      { label: "Security suite", href: "/features/security/" },
      { label: "Real User Monitoring", href: "/features/real-user-monitoring/" },
    ],
  },
  {
    version: "0.61.33",
    date: "2026-07-07",
    summary: "Your own backup destinations now actually work, end to end.",
    items: [
      {
        tag: "Fixed",
        text: "Configuring a local folder or your own S3-compatible bucket as a backup destination looked like it worked (the \"Test connection\" check passed), but every backup still went to managed storage regardless. Backups, full and incremental, now run to the destination you configured, and restore reads back from it, across all three types: managed storage, a local folder on the server, and your own S3-compatible bucket. For your own bucket, the control plane signs the uploads and downloads so the site never holds your storage credentials.",
      },
      {
        tag: "Fixed",
        text: "Temporary backup working files left behind by a failed or interrupted run are now cleaned up automatically by a daily job, instead of slowly accumulating on disk.",
      },
      {
        tag: "Changed",
        text: "Hosted service only: managed backup storage is now a paid-plan feature. On the free plan, backups target your own storage (a local folder or your own S3-compatible bucket) instead. Self-hosted installs are unaffected and always keep managed storage, and restoring an existing backup is never restricted.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
  },
  {
    version: "0.61.31",
    date: "2026-07-07",
    summary: "Restore now verifies the site actually loads, and rolls back automatically if it does not.",
    items: [
      {
        tag: "Added",
        text: "After the files and database are swapped, restore now runs two checks: first that the restored database is intact while the site is still in maintenance mode, then, once the site is live, that it is not showing a fatal error. If either check finds a genuine failure, the restore automatically reverts both the files and the database to their pre-restore state and reports the run as failed with the reason, instead of leaving a broken site behind and reporting success. The checks fail open on a network blip, so they can never roll back a good restore.",
      },
      {
        tag: "Fixed",
        text: "Restore could also silently drop plugin or theme files whose path happened to contain a reserved drop-in name (for example a plugin's own class-db.php), leaving the site broken while the restore reported success. Restore now matches its protected-file exclusions by exact path instead of substring, so only genuine WordPress root drop-ins and config files are held back. Sites affected by an earlier restore can recover by re-restoring from the same snapshot.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
  },
  {
    version: "0.61.30",
    date: "2026-07-07",
    summary: "Uptime incidents now have real history, with a detail view.",
    items: [
      {
        tag: "Added",
        text: "Uptime incidents are now recorded and kept, so the Incidents panel shows real history: past incidents with accurate durations and a flapping indicator for sites that go down repeatedly. Clicking an incident opens a detail view with the site's live status, the probe results across the incident window, a timeline of what else happened around that time (updates, backups, activity, PHP errors), 7- and 30-day uptime, and quick actions.",
      },
      {
        tag: "Fixed",
        text: "An ongoing incident previously showed the wrong severity (\"Degraded\" for a site that was down), a duration that read \"NaNh\", and a blank site name; incidents now show the correct severity, read \"ongoing\" while open, and always show the site name. Also fixed: unreadable dropdown menus in dark mode, two dead breadcrumb links, and an \"Open site\" action that did nothing.",
      },
    ],
    featureLinks: [{ label: "Uptime monitoring", href: "/features/uptime-monitoring/" }],
  },
  {
    version: "0.61.26",
    date: "2026-07-06",
    summary: "Uptime: TLS expiry now shows, and downtime alert emails send reliably.",
    items: [
      {
        tag: "Fixed",
        text: "TLS certificate expiry now shows for monitored sites. The uptime prober only read the certificate during a fresh TLS handshake, which almost never happened after the first probe on a reused connection, so the TLS expiry column stayed empty from the start. It now reads the certificate on every probe, fresh or reused.",
      },
      {
        tag: "Fixed",
        text: "Downtime and recovery alert emails were silently skipped when SMTP was configured only in the dashboard (Settings, Email/SMTP) and not also set as environment variables. Alerts now send through the same saved relay used everywhere else, with environment variables kept only as a fallback.",
      },
      {
        tag: "Fixed",
        text: "The audit log recorded \"Emailed: Yes\" for a downtime alert whenever recipients were configured, even when the send was skipped or failed. It now records the true outcome (Sent, Skipped, or Failed) with the reason, for both email and webhook delivery.",
      },
    ],
    featureLinks: [{ label: "Uptime monitoring", href: "/features/uptime-monitoring/" }],
  },
  {
    version: "0.61.17",
    date: "2026-07-05",
    summary: "Bulk, chain-aware backup deletion.",
    items: [
      {
        tag: "Added",
        text: "Select multiple backups with checkboxes, including whole incremental chains via a tri-state chain checkbox, with one-click filters for all failed or all zero-byte runs, and delete the batch in one action. Selecting a snapshot automatically includes the later generations that depend on it, shown before you confirm, so a chain can never be left broken.",
      },
      {
        tag: "Added",
        text: "A batch of failed or zero-byte runs needs one plain confirmation; a batch containing any completed backup asks you to type one phrase for the whole batch. Deleting a snapshot, single or bulk, now also refuses while a restore that reads it is in progress.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
  },
  {
    version: "0.61.15",
    date: "2026-07-04",
    summary: "Fleet Audit log redesigned, with several reliability fixes.",
    items: [
      {
        tag: "Changed",
        text: "Redesigned the fleet Audit log. Events now read as plain sentences instead of raw internal codes, the operator who performed each action is shown by name, and long runs of routine file reads collapse into one expandable line so writes, deletions, and denied actions stand out. Added an outcome filter (Denied, Writes, Sensitive), a search box, exact timestamps, and a per-event detail view.",
      },
      {
        tag: "Fixed",
        text: "The event list was ordered oldest-first while presented as newest-first, so once a tenant passed one page of events, recent activity was paged off the end; it now lists newest events first (no audit data was ever lost). Also fixed a false \"Chain break\" integrity warning caused by two actions recorded at the same instant, and a page-load failure for accounts with automated activity like uptime alerts and backups.",
      },
      {
        tag: "Added",
        text: "Owners can now acknowledge a \"Chain break\" that predates an older fix, since the audit log is append-only and its rows can never be altered. Acknowledging moves the integrity anchor to the current point so verification runs forward cleanly; new tampering is still detected, and the acknowledgment itself is recorded in the audit log.",
      },
    ],
    featureLinks: [{ label: "Team and access control", href: "/features/team-access/" }],
  },
  {
    version: "0.61.10",
    date: "2026-07-02",
    summary: "Bulk updates only touch sites that actually need them.",
    items: [
      {
        tag: "Fixed",
        text: "Multi-site bulk update now only creates a task for a site that actually has the selected update pending, instead of also creating a task, and then failing it, for every selected site that does not have that plugin, theme, or core update available. Sites the update does not apply to are now reported as skipped, not failed.",
      },
    ],
  },
  {
    version: "0.61.9",
    date: "2026-07-02",
    summary: "A failed update no longer strands a site in maintenance mode.",
    items: [
      {
        tag: "Fixed",
        text: "A failed plugin or theme update no longer leaves a site stuck showing WordPress's maintenance page to every visitor. The post-update health check now tolerates a brief, transient 503 from an in-progress database migration instead of treating it as a failure and rolling back an update that actually succeeded.",
      },
    ],
  },
  {
    version: "0.61.6",
    date: "2026-07-02",
    summary: "Reliable downtime alerts and a faster Sites list.",
    items: [
      {
        tag: "Fixed",
        text: "Downtime email alerts now fire reliably. A race in the alert state machine could let a sustained outage go unalerted; alert state now transitions atomically so every qualifying outage sends its alert.",
      },
      {
        tag: "Fixed",
        text: "The Notifications settings page now saves correctly, including the daily email digest toggle.",
      },
      {
        tag: "Fixed",
        text: "Fixed a slow first load of the Sites list after the dashboard had been idle for a while.",
      },
    ],
  },
  {
    version: "0.61.3",
    date: "2026-06-26",
    summary: "A batch of dashboard, backup, and update fixes.",
    items: [
      {
        tag: "Fixed",
        text: "Backups now correctly preserve plugin and theme vendor code that happens to live in a folder named cache or upgrade, while still excluding real runtime cache and update staging files.",
      },
      {
        tag: "Fixed",
        text: "The backup schedule form no longer rejects valid day-of-week, day-of-month, or hourly-interval selections, and update run history now shows accurate task counts and completion progress.",
      },
      {
        tag: "Fixed",
        text: "Closing any dialog no longer leaves the page unclickable, and bulk plugin and theme updates now default to only the items that actually have an update available.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
  },
  {
    version: "0.61.0",
    date: "2026-06-23",
    summary: "File Manager for every managed site.",
    items: [
      {
        tag: "Added",
        text: "Browse, preview, edit, upload, download, zip, and restore prior versions of files on any managed WordPress site, right from the dashboard, no SFTP or cPanel required. A sensitive-file deny list and a separate write-access toggle keep it safe, and every action is written to the audit log. Off by default; only owners and admins can turn it on.",
      },
      {
        tag: "Added",
        text: "CloudPanel sites now get integrated cache purging: WPMgr clears its own page cache and CloudPanel's Varnish cache together, no separate plugin needed.",
      },
    ],
    featureLinks: [{ label: "File Manager", href: "/features/file-manager/" }],
  },
  {
    version: "0.57.7",
    date: "2026-06-21",
    summary: "New multipage marketing website.",
    items: [
      {
        tag: "Changed",
        text: "The public site at wpmgr.app is now a multipage, server-rendered site: a dedicated page for every feature, solution pages by audience and by job, pricing, this changelog, a resources area, and a self-hosted API reference generated from the OpenAPI spec. Faster, fully crawlable, and easier to extend.",
      },
    ],
  },
  {
    version: "0.57.0",
    date: "2026-06-21",
    summary: "Vulnerability feed configuration from the dashboard.",
    items: [
      {
        tag: "Added",
        text: "Instance administrators can configure the Wordfence Intelligence API key from a new admin page instead of an environment variable. The page shows live connection status, lets you save or remove the key, and provides a Sync now action. The key is encrypted at rest.",
      },
    ],
    featureLinks: [{ label: "Security suite", href: "/features/security/" }],
  },
  {
    version: "0.56.0",
    date: "2026-06-20",
    summary: "Vulnerability scanner across your fleet.",
    items: [
      {
        tag: "Added",
        text: "WPMgr now checks every managed site's plugins, themes, and WordPress core against the Wordfence Intelligence vulnerability feed. Each finding shows severity, affected version range, fixed version, and CVE references. One-click remediation updates the vulnerable item using the existing update flow. Findings appear per-site on the Security tab and fleet-wide on the Vulnerabilities page.",
      },
    ],
    featureLinks: [{ label: "Security suite", href: "/features/security/" }],
  },
  {
    version: "0.55.0",
    date: "2026-06-20",
    summary: "2FA enrollment flow for site users + redesigned Security tab.",
    items: [
      {
        tag: "Added",
        text: "After an operator requires 2FA for a user role, affected users now see a guided enrollment screen on next login: scan a QR code, confirm a code, save backup codes. Users can also start enrollment proactively from their WordPress profile.",
      },
      {
        tag: "Changed",
        text: "The per-site Security tab is now a card-based layout with a status overview strip and collapsible setting groups: Login and Two-Factor, Password policy, Hardening, File integrity, Bans, and Hide login.",
      },
    ],
    featureLinks: [
      { label: "Two-factor auth", href: "/features/two-factor-auth/" },
      { label: "Security suite", href: "/features/security/" },
    ],
  },
  {
    version: "0.54.0",
    date: "2026-06-20",
    summary: "2FA for WordPress site users, password policy, and hidden login.",
    items: [
      {
        tag: "Added",
        text: "Operators can require 2FA for chosen user roles, enforced at the WordPress login screen. Methods: authenticator app (TOTP), email one-time code, backup codes. A grace period lets users enroll before enforcement. The control plane and wp-config bypass can never be locked out.",
      },
      {
        tag: "Added",
        text: "Per-site password policy: minimum strength, known-compromised password blocking (privacy-preserving prefix query), reuse blocking, and optional expiry.",
      },
      {
        tag: "Added",
        text: "Hide login page: move wp-login.php to a secret address per site. All three controls are per-site, opt-in, and off by default.",
      },
    ],
    featureLinks: [{ label: "Two-factor auth", href: "/features/two-factor-auth/" }],
  },
];

function formatDate(iso: string) {
  return new Date(iso).toLocaleDateString("en-US", {
    year: "numeric",
    month: "long",
    day: "numeric",
  });
}

export default function ChangelogPage() {
  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Changelog", href: "/changelog/" },
  ]);

  return (
    <>
      <JsonLd data={breadcrumb} />

      {/* Hero */}
      <section className="border-b border-[var(--border)] py-16 sm:py-20">
        <Container>
          <div className="max-w-2xl">
            <p className="mb-3 text-sm font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
              Changelog
            </p>
            <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
              What shipped and when
            </h1>
            <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
              Every WPMgr release, newest first. Each entry links to the relevant feature pages.
              For the full history and release artifacts, see{" "}
              <a
                href={`${SITE_CONFIG.github}/releases`}
                target="_blank"
                rel="noreferrer noopener"
                className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
              >
                GitHub Releases
              </a>
              .
            </p>
          </div>
        </Container>
      </section>

      {/* Release feed */}
      <Section>
        <Container className="max-w-4xl">
          <div className="relative">
            {/* Timeline spine */}
            <div
              aria-hidden
              className="absolute left-[15px] top-0 bottom-0 w-px bg-[var(--border)] sm:left-[19px]"
            />

            <ol className="space-y-12" aria-label="Release history">
              {RELEASES.map((release) => (
                <li key={release.version} className="relative pl-10 sm:pl-14">
                  {/* Dot */}
                  <div
                    aria-hidden
                    className="absolute left-0 top-1.5 h-[30px] w-[30px] sm:h-[38px] sm:w-[38px] flex items-center justify-center rounded-full border-2 border-[var(--border)] bg-[var(--background)] text-[10px] font-bold text-[var(--primary)]"
                  >
                    <span className="hidden sm:inline text-[9px]">v</span>
                    <span className="text-[8px] sm:text-[8px] leading-none">
                      {release.version.split(".").slice(0, 2).join(".")}
                    </span>
                  </div>

                  {/* Content */}
                  <div>
                    <div className="flex flex-wrap items-baseline gap-3">
                      <a
                        href={`${SITE_CONFIG.github}/releases/tag/v${release.version}`}
                        target="_blank"
                        rel="noreferrer noopener"
                        className="font-mono text-lg font-semibold text-foreground hover:text-[var(--primary)] transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm"
                      >
                        v{release.version}
                      </a>
                      <time
                        dateTime={release.date}
                        className="text-sm text-[var(--muted-foreground)]"
                      >
                        {formatDate(release.date)}
                      </time>
                    </div>

                    <p className="mt-1 font-medium text-foreground">{release.summary}</p>

                    <ul className="mt-4 space-y-3" aria-label="Changes in this release">
                      {release.items.map((item, i) => (
                        <li key={i} className="flex items-start gap-3">
                          <span
                            className="mt-0.5 inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-semibold"
                            style={{
                              background: `color-mix(in oklch, ${TAG_COLOR[item.tag]} 15%, transparent)`,
                              color: TAG_COLOR[item.tag],
                            }}
                          >
                            {item.tag}
                          </span>
                          <span className="text-sm leading-relaxed text-[var(--muted-foreground)]">
                            {item.text}
                          </span>
                        </li>
                      ))}
                    </ul>

                    {release.featureLinks && release.featureLinks.length > 0 && (
                      <div className="mt-4 flex flex-wrap gap-3">
                        {release.featureLinks.map((link) => (
                          <Link
                            key={link.href}
                            href={link.href}
                            className="inline-flex items-center gap-1.5 rounded-full border border-[var(--border)] bg-card px-3 py-1 text-xs font-medium text-[var(--muted-foreground)] transition-colors hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                          >
                            {link.label}
                          </Link>
                        ))}
                      </div>
                    )}
                  </div>
                </li>
              ))}
            </ol>
          </div>

          {/* Full history link */}
          <div className="mt-14 rounded-xl border border-[var(--border)] bg-[var(--muted)]/40 p-6 text-center">
            <p className="text-[var(--muted-foreground)]">
              This page shows the most recent releases. For the complete release history and all
              release artifacts:
            </p>
            <a
              href={`${SITE_CONFIG.github}/releases`}
              target="_blank"
              rel="noreferrer noopener"
              className="mt-3 inline-flex items-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-5 py-2.5 text-sm font-medium text-foreground shadow-sm transition-colors hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
            >
              View all releases on GitHub
            </a>
          </div>
        </Container>
      </Section>
    </>
  );
}
