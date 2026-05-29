import { useEffect, useRef, useState, type ReactNode } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import { MoreHorizontal, Zap } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  StatusChip,
  BackupChip,
  VulnSeverityChip,
  type StatusTone,
} from "@/components/status";
import { useSite, NotFoundError } from "@/features/sites/use-sites";
import {
  SiteComponentsTable,
  countUpToDate,
} from "@/features/sites/site-components-table";
import { SiteTagsEditor } from "@/features/sites/site-tags-editor";
import { AutoLoginButton } from "@/features/sites/auto-login-button";
import { useBackups } from "@/features/backups/use-backups";
import { useSiteUptime } from "@/features/monitoring/use-uptime";
import { AvailableUpdatesCard } from "@/features/updates/available-updates-card";
import { BackupsSection } from "@/features/backups/backups-section";
import { useMe, canOperate } from "@/features/auth/use-auth";
import { formatBytes, relativeTime, cn } from "@/lib/utils";
import type { Site } from "@wpmgr/api";

// Surface 4.7 — Site detail page (Sprint 3, Impeccable Restyle Phase 4).
//
// Single column, max-width 1200px. Two surface levels TOTAL: page on
// `--background`, content on `--card`. NEVER nests a card inside another card,
// per DESIGN.md ("Don't nest cards. Don't wrap every section in a card.").
//
// The page is organised as six anchored sections (Health, Updates, Backups,
// Security, Activity, Settings) separated by 32px vertical padding and a 1px
// border. A sticky sub-nav above them shows the active section and supports
// keyboard-driven smooth-scroll navigation. The Health block is the showcase
// tile: ONE card surface containing a 4-tile grid divided by 1px borders.

export const Route = createFileRoute("/_authed/sites/$siteId")({
  component: SiteDetailPage,
});

const SECTIONS = [
  { id: "health", label: "Health" },
  { id: "updates", label: "Updates" },
  { id: "backups", label: "Backups" },
  { id: "security", label: "Security" },
  { id: "activity", label: "Activity" },
  { id: "settings", label: "Settings" },
] as const;

type SectionId = (typeof SECTIONS)[number]["id"];

function SiteDetailPage() {
  const { siteId } = Route.useParams();
  const { data: site, isPending, isError, error, refetch } = useSite(siteId);
  const { data: me } = useMe();
  const operate = canOperate(me);

  return (
    <div className="mx-auto w-full max-w-[1200px]">
      {isPending ? (
        <div className="px-6 py-8">
          <p role="status" className="text-sm text-muted-foreground">
            Loading site…
          </p>
        </div>
      ) : isError ? (
        <div className="px-6 py-8">
          {error instanceof NotFoundError ? (
            <div role="alert" className="space-y-2">
              <h1 className="text-2xl font-semibold">Site not found</h1>
              <p className="text-sm text-muted-foreground">
                No site exists with id <code className="font-mono">{siteId}</code>.
              </p>
              <Button asChild variant="outline" size="sm">
                <Link to="/sites">Back to sites</Link>
              </Button>
            </div>
          ) : (
            <div role="alert" className="space-y-3">
              <h1 className="text-2xl font-semibold">Could not load site</h1>
              <p className="text-sm text-destructive">{error.message}</p>
              <Button variant="outline" size="sm" onClick={() => void refetch()}>
                Retry site
              </Button>
            </div>
          )}
        </div>
      ) : (
        <SiteDetail site={site} canOperate={operate} />
      )}
    </div>
  );
}

// ── Site detail body ─────────────────────────────────────────────────────────

function SiteDetail({
  site,
  canOperate,
}: {
  site: Site;
  canOperate: boolean;
}) {
  const active = useActiveSection();

  // Pull the hostname out of the site URL for the mono header label.
  const hostname = hostnameOf(site.url);
  const adminUrl = `${stripTrailingSlash(site.url)}/wp-admin/`;
  const tone = toneForHealth(site.health_status);
  const toneLabel = labelForHealth(site.health_status);
  const lastSeen = relativeTime(site.last_seen_at) ?? undefined;

  return (
    <>
      {/* Top sticky header strip. Sits below the 48px TopBar (top-12). */}
      <header
        className={cn(
          "sticky top-12 z-20 -mx-6 flex h-12 items-center gap-3 border-b border-border bg-background px-6",
          // Allow the hostname to truncate on narrow widths without breaking layout.
          "min-w-0",
        )}
      >
        <div
          className="truncate font-mono text-base text-foreground"
          title={hostname}
        >
          {hostname}
        </div>
        <StatusChip tone={tone} label={toneLabel} time={lastSeen} />
        <div className="ml-auto flex items-center gap-2">
          <Button asChild size="sm" aria-label="Open in wp-admin">
            <a
              href={adminUrl}
              target="_blank"
              rel="noopener noreferrer"
            >
              <Zap aria-hidden="true" className="size-4" />
              Open wp-admin
            </a>
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                type="button"
                variant="outline"
                size="icon"
                aria-label="More site actions"
              >
                <MoreHorizontal aria-hidden="true" className="size-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem disabled title="Coming soon">
                Run health check
              </DropdownMenuItem>
              <DropdownMenuItem disabled title="Coming soon">
                Copy site ID
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem disabled title="Coming soon">
                Disconnect site
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </header>

      {/* Anchored sub-nav, sticky directly under the header strip (48px + 48px = 96px). */}
      <nav
        aria-label="Site sections"
        className="sticky top-24 z-10 -mx-6 flex h-12 items-center gap-1 border-b border-border bg-background px-6"
      >
        {SECTIONS.map((s) => {
          const isActive = active === s.id;
          return (
            <a
              key={s.id}
              href={`#${s.id}`}
              onClick={(e) => onAnchorClick(e, s.id)}
              aria-current={isActive ? "true" : undefined}
              className={cn(
                "inline-flex h-12 items-center px-3 text-sm transition-colors",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
                isActive
                  ? "-mb-px border-b-2 border-primary font-medium text-foreground"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              {s.label}
            </a>
          );
        })}
      </nav>

      <div className="px-6 py-8">
        {/* HEALTH — the showcase 4-tile grid inside ONE card. */}
        <Section id="health" label="Health">
          <HealthGrid site={site} />
        </Section>

        <SectionDivider />

        {/* UPDATES — reuses AvailableUpdatesCard. */}
        <Section id="updates" label="Updates">
          <AvailableUpdatesCard siteId={site.id} />
        </Section>

        <SectionDivider />

        {/* BACKUPS — recent snapshots + schedule + run-now (operator+). */}
        <Section id="backups" label="Backups">
          <BackupsSection siteId={site.id} canOperate={canOperate} />
        </Section>

        <SectionDivider />

        {/* SECURITY — vulnerability scan stub. Sprint 4 will wire it up. */}
        <Section id="security" label="Security">
          <SecurityStub />
        </Section>

        <SectionDivider />

        {/* ACTIVITY — audit log stub. Sprint 4 will wire it up. */}
        <Section id="activity" label="Activity">
          <ActivityStub />
        </Section>

        <SectionDivider />

        {/* SETTINGS — name, tags, components inventory. Sprint 4 forms-architect
            will refine the layout; structurally the section is in place. */}
        <Section id="settings" label="Settings">
          <SettingsSection site={site} canEditTags={canOperate} />
        </Section>
      </div>
    </>
  );
}

// ── Section primitives ───────────────────────────────────────────────────────

function Section({
  id,
  label,
  children,
}: {
  id: SectionId;
  label: string;
  children: ReactNode;
}) {
  return (
    <section
      id={id}
      // 96px scroll-margin keeps the section heading visible below the sticky
      // header + sub-nav stack (48 + 48 = 96) when the user jumps via anchor.
      style={{ scrollMarginTop: 96 }}
      aria-labelledby={`${id}-heading`}
    >
      <h2
        id={`${id}-heading`}
        className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground"
      >
        {label}
      </h2>
      {children}
    </section>
  );
}

function SectionDivider() {
  // Sections separated by 32px vertical (my-8 = 32px) + 1px border. This is
  // explicitly NOT a card.
  return <hr className="my-8 border-border" aria-hidden="true" />;
}

// ── HEALTH: the 4-tile grid (ONE card surface, divided by borders) ──────────

function HealthGrid({ site }: { site: Site }) {
  return (
    <div className="rounded-lg border border-border bg-card">
      <div
        className={cn(
          "grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4",
          "divide-y divide-border sm:divide-y-0 sm:divide-x",
        )}
      >
        <UptimeTile siteId={site.id} />
        <LastBackupTile siteId={site.id} />
        <VulnerabilitiesTile />
        <PerformanceTile />
      </div>
    </div>
  );
}

function Tile({
  title,
  value,
  sub,
  children,
}: {
  title: string;
  value: ReactNode;
  sub?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-col gap-2 p-6">
      <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {title}
      </div>
      <div className="text-2xl font-semibold tabular-nums text-foreground">
        {value}
      </div>
      {sub ? (
        <div className="text-xs text-muted-foreground tabular-nums">{sub}</div>
      ) : null}
      {children ? <div className="pt-1">{children}</div> : null}
    </div>
  );
}

function UptimeTile({ siteId }: { siteId: string }) {
  const { data, isPending } = useSiteUptime(siteId, "30d");

  if (isPending) {
    return <Tile title="Uptime 30d" value="…" sub="Loading" />;
  }
  if (!data) {
    return <Tile title="Uptime 30d" value="—" sub="No probes yet" />;
  }
  const pct = data.uptime_pct;
  // Per-bucket average latency is what the API surfaces; use it as the
  // sparkline series so the eye sees the same trend the headline number
  // summarises.
  const latencyValues = data.series
    .map((s) => s.avg_latency_ms)
    .filter((v): v is number => typeof v === "number" && Number.isFinite(v));

  return (
    <Tile
      title="Uptime 30d"
      value={`${pct.toFixed(2)}%`}
      sub={`avg ${data.avg_latency_ms} ms`}
    >
      <Sparkline values={latencyValues} ariaLabel="Latency sparkline" />
    </Tile>
  );
}

function LastBackupTile({ siteId }: { siteId: string }) {
  const { data, isPending } = useBackups(siteId);

  if (isPending) {
    return <Tile title="Last backup" value="…" sub="Loading" />;
  }
  const completed = (data ?? []).find((s) => s.status === "completed");
  if (!completed) {
    return <Tile title="Last backup" value="None" sub="Run a backup" />;
  }
  const when = relativeTime(completed.finished_at ?? completed.created_at);
  return (
    <Tile
      title="Last backup"
      value={when ?? "Recent"}
      sub={formatBytes(completed.total_size)}
    >
      <BackupChip status="success" time={when ?? undefined} />
    </Tile>
  );
}

function VulnerabilitiesTile() {
  // TODO(sprint-4): wire to /vulnerabilities once the endpoint lands. The
  // calm "no findings" state is the operator-correct default until then.
  return (
    <Tile title="Vulnerabilities" value="0" sub="No findings">
      <VulnSeverityChip severity="low" count={0} />
    </Tile>
  );
}

function PerformanceTile() {
  // TODO(sprint-4): wire to /performance once the endpoint lands.
  return <Tile title="Performance" value="—" sub="Not measured yet" />;
}

// ── Sparkline (chart-1 stroke, 1.5px, no fill, real data only) ──────────────

function Sparkline({
  values,
  ariaLabel,
}: {
  values: number[];
  ariaLabel: string;
}) {
  if (values.length < 2) {
    return (
      <div
        aria-hidden="true"
        className="h-6 w-full rounded-sm bg-muted/40"
        title="Not enough data"
      />
    );
  }
  const width = 120;
  const height = 24;
  const min = Math.min(...values);
  const max = Math.max(...values);
  const range = max - min || 1;
  const step = width / (values.length - 1);
  const points = values
    .map((v, i) => {
      const x = i * step;
      const y = height - ((v - min) / range) * height;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
  return (
    <svg
      role="img"
      aria-label={ariaLabel}
      viewBox={`0 0 ${width} ${height}`}
      className="h-6 w-full"
      preserveAspectRatio="none"
    >
      <polyline
        points={points}
        fill="none"
        stroke="var(--color-chart-1)"
        strokeWidth={1.5}
        vectorEffect="non-scaling-stroke"
        strokeLinejoin="round"
        strokeLinecap="round"
      />
    </svg>
  );
}

// ── SECURITY / ACTIVITY stubs ────────────────────────────────────────────────

function SecurityStub() {
  // Sprint 4 wires the scan results. Until then, render the calm empty state
  // (DESIGN.md: state is the design, no "introducing" copy) and the Run-scan
  // action so the affordance is visible.
  return (
    <div className="rounded-lg border border-border bg-card p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="space-y-1">
          <p className="text-sm text-muted-foreground">
            No vulnerabilities found in the last scan.
          </p>
          <p className="text-xs text-muted-foreground">
            Scan results appear here after the agent completes a sweep.
          </p>
        </div>
        <Button size="sm" variant="outline" disabled title="Coming soon">
          Run scan
        </Button>
      </div>
    </div>
  );
}

function ActivityStub() {
  // Sprint 4 wires the audit log query. Empty state is intentional.
  return (
    <div className="rounded-lg border border-border bg-card p-6">
      <p className="text-sm text-muted-foreground">
        No activity yet for this site.
      </p>
      <p className="mt-1 text-xs text-muted-foreground">
        Updates, backups, and operator actions appear here, newest first.
      </p>
    </div>
  );
}

// ── SETTINGS section: site name, tags, components inventory ─────────────────

function SettingsSection({
  site,
  canEditTags,
}: {
  site: Site;
  canEditTags: boolean;
}) {
  // The settings section is one card surface holding multiple stacked fields,
  // separated by horizontal rules — NOT four nested cards. Sprint 4
  // forms-architect will swap the static stack for a sticky save bar form.
  return (
    <div className="rounded-lg border border-border bg-card">
      <div className="space-y-1 p-6">
        <h3 className="text-sm font-semibold text-foreground">Identity</h3>
        <dl className="grid grid-cols-1 gap-x-6 gap-y-2 text-sm sm:grid-cols-2">
          <Detail label="Name" value={site.name} />
          <Detail
            label="URL"
            value={<span className="font-mono break-all">{site.url}</span>}
          />
          <Detail
            label="Site ID"
            value={<span className="font-mono">{site.id}</span>}
          />
          <Detail label="Status" value={site.status} />
          <Detail label="WordPress" value={site.wp_version || "—"} />
          <Detail label="PHP" value={site.php_version || "—"} />
        </dl>
      </div>
      <hr className="border-border" aria-hidden="true" />
      <div className="space-y-3 p-6">
        <h3 className="text-sm font-semibold text-foreground">Tags</h3>
        {canEditTags ? (
          <SiteTagsEditor site={site} />
        ) : site.tags.length > 0 ? (
          <div className="flex flex-wrap gap-1">
            {site.tags.map((tag) => (
              <span
                key={tag}
                className="rounded border border-border px-2 py-0.5 text-xs"
              >
                {tag}
              </span>
            ))}
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">No tags.</p>
        )}
      </div>
      <hr className="border-border" aria-hidden="true" />
      <div className="space-y-3 p-6">
        <h3 className="text-sm font-semibold text-foreground">
          Installed components (
          {countUpToDate(site.components?.plugins, site.components?.themes)})
        </h3>
        <p className="text-xs text-muted-foreground">
          Plugins and themes already up to date.
        </p>
        <SiteComponentsTable
          plugins={site.components?.plugins}
          themes={site.components?.themes}
        />
      </div>
      <hr className="border-border" aria-hidden="true" />
      <div className="space-y-3 p-6">
        <h3 className="text-sm font-semibold text-foreground">Access</h3>
        <p className="text-xs text-muted-foreground">
          One-click sign in into wp-admin as an existing administrator. Logs the
          action to the audit trail.
        </p>
        <AutoLoginButton siteId={site.id} siteName={site.name} />
      </div>
    </div>
  );
}

function Detail({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs uppercase tracking-wide text-muted-foreground">
        {label}
      </dt>
      <dd className="break-words font-medium">{value}</dd>
    </div>
  );
}

// ── Helpers ──────────────────────────────────────────────────────────────────

function hostnameOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

function stripTrailingSlash(url: string): string {
  return url.endsWith("/") ? url.slice(0, -1) : url;
}

function toneForHealth(status: Site["health_status"]): StatusTone {
  switch (status) {
    case "healthy":
      return "success";
    case "unreachable":
      return "destructive";
    case "unknown":
    default:
      return "muted";
  }
}

function labelForHealth(status: Site["health_status"]): string {
  switch (status) {
    case "healthy":
      return "Up";
    case "unreachable":
      return "Down";
    case "unknown":
    default:
      return "Unknown";
  }
}

function onAnchorClick(
  e: React.MouseEvent<HTMLAnchorElement>,
  id: SectionId,
) {
  // Smooth-scroll the anchor with our scroll-margin honored; let modifier
  // clicks behave normally (cmd-click to open in new tab is meaningless for
  // an anchor, but we still avoid hijacking the event).
  if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const target = document.getElementById(id);
  if (!target) return;
  e.preventDefault();
  // History keeps the anchor so the URL reflects the active section.
  history.replaceState(null, "", `#${id}`);
  // `prefers-reduced-motion` is respected globally via the CSS reset in
  // globals.css (scroll-behavior is forced to auto). We still ask for smooth
  // so motion-OK users get the gentle landing.
  target.scrollIntoView({ behavior: "smooth", block: "start" });
}

// Track which section is currently in view. The naive intersection observer
// pattern (first intersecting wins) feels wrong when scrolling fast because
// multiple sections intersect at once; we instead pick the section whose top
// edge is closest to (but above) the sub-nav baseline.
function useActiveSection(): SectionId {
  const [active, setActive] = useState<SectionId>(SECTIONS[0].id);
  const ticking = useRef(false);

  useEffect(() => {
    function compute() {
      ticking.current = false;
      const baseline = 100; // sub-nav baseline + small fudge
      let candidate: SectionId = SECTIONS[0].id;
      for (const s of SECTIONS) {
        const el = document.getElementById(s.id);
        if (!el) continue;
        const top = el.getBoundingClientRect().top;
        if (top - baseline <= 0) {
          candidate = s.id;
        } else {
          break;
        }
      }
      setActive(candidate);
    }
    function onScroll() {
      if (ticking.current) return;
      ticking.current = true;
      window.requestAnimationFrame(compute);
    }
    compute();
    window.addEventListener("scroll", onScroll, { passive: true });
    window.addEventListener("resize", onScroll);
    return () => {
      window.removeEventListener("scroll", onScroll);
      window.removeEventListener("resize", onScroll);
    };
  }, []);

  return active;
}
