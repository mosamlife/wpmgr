import { useEffect, useState } from "react";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { DefinitionList } from "@/components/shared/definition-list";
import { LiveIndicator } from "@/components/shared/live-indicator";
import { useBackup, NotFoundError } from "@/features/backups/use-backups";
import { StatusBadge, KindBadge } from "@/features/backups/backup-badges";
import { RestoreDialog } from "@/features/backups/restore-dialog";
import { SnapshotProgressCard } from "@/features/backups/snapshot-progress-card";
import { SqlInspectionCard } from "@/features/backups/sql-inspection-card";
import { ManifestCard } from "@/features/backups/manifest-card";
import { useSqlInspection } from "@/features/backups/use-sql-inspection";
import { useBackupStream } from "@/features/backups/use-backup-stream";
import { isRestoreActive } from "@/features/backups/format-progress";
import {
  useSnapshotEnvironment,
  type EnvFingerprint,
} from "@/features/backups/use-snapshot-environment";
import { useMe, canOperate } from "@/features/auth/use-auth";
import { useSite } from "@/features/sites/use-sites";
import { cn, formatBytes, relativeTime } from "@/lib/utils";
import type { BackupSnapshot, BackupSnapshotDetail } from "@wpmgr/api";

export const Route = createFileRoute("/_authed/backups/$snapshotId")({
  component: SnapshotDetailPage,
});

function SnapshotDetailPage() {
  const { snapshotId } = Route.useParams();
  const { data, isPending, isError, error, refetch } = useBackup(snapshotId);
  const { data: me } = useMe();
  const operate = canOperate(me);

  if (isPending) {
    return <DetailSkeleton />;
  }

  if (isError) {
    if (error instanceof NotFoundError) {
      return (
        <section aria-labelledby="snapshot-heading" className="space-y-4">
          <Button asChild variant="outline" size="sm">
            <Link to="/sites">Back to sites</Link>
          </Button>
          <div role="alert" className="space-y-2">
            <h1 id="snapshot-heading" className="text-2xl font-semibold">
              Snapshot not found
            </h1>
            <p className="text-muted-foreground">
              No backup snapshot exists with id{" "}
              <code className="font-mono">{snapshotId}</code>. It may have aged
              out of retention or been deleted.
            </p>
          </div>
        </section>
      );
    }
    return (
      <section aria-labelledby="snapshot-heading" className="space-y-4">
        <Button asChild variant="outline" size="sm">
          <Link to="/sites">Back to sites</Link>
        </Button>
        <PageError
          what="Could not load this snapshot."
          why={error.message}
          onRetry={() => void refetch()}
          retryLabel="Reload snapshot"
        />
      </section>
    );
  }

  return <SnapshotDetailView detail={data} canRestore={operate} />;
}

function DetailSkeleton() {
  return (
    <div className="space-y-6" role="status" aria-label="Loading snapshot">
      <div className="space-y-2">
        <Skeleton className="h-3.5 w-32" />
        <Skeleton className="h-6 w-64" />
        <Skeleton className="h-4 w-96" />
      </div>
      <div className="grid gap-6 lg:grid-cols-[2fr_1fr]">
        <Skeleton className="h-72 w-full rounded-xl" />
        <Skeleton className="h-72 w-full rounded-xl" />
      </div>
    </div>
  );
}

/** True when snapshot.progress carries an active restore phase. Delegates to
 *  the shared helper, which gates on the restore PHASE (not on
 *  formatProgress().isTerminal — that flag is poisoned by status==="completed",
 *  which is the steady state during a restore overlay). */
function hasActiveRestorePhase(snapshot: BackupSnapshot): boolean {
  return isRestoreActive(snapshot);
}

/** True when this snapshot is a running backup OR an in-flight restore. */
function isInFlight(snapshot: BackupSnapshot): boolean {
  return snapshot.status === "running" || hasActiveRestorePhase(snapshot);
}

function SnapshotDetailView({
  detail,
  canRestore,
}: {
  detail: BackupSnapshotDetail;
  canRestore: boolean;
}) {
  const { snapshot, entries } = detail;
  const [restoreOpen, setRestoreOpen] = useState(false);
  const [restoreRequestedAt, setRestoreRequestedAt] = useState<number | null>(
    null,
  );
  const navigate = useNavigate();

  // Resolve the originating site so the back-link returns to the right
  // Backups tab (not the global sites list) and so the destructive-confirm
  // can use the real host.
  const { data: site } = useSite(snapshot.site_id);

  // ALWAYS open the SSE stream on this page so the first restore event lands
  // and triggers the progress card to render. The returned state is consumed
  // by SnapshotProgressCard via its own hook call; this call is for the side
  // effect of opening the EventSource.
  useBackupStream(snapshot.id);

  // SQL inspection is now first-class page content (was only inside the
  // restore modal). We read it once here so the manifest can merge per-table
  // row/byte/charset data; SqlInspectionCard reads the same query key and
  // dedupes through TanStack Query.
  const sqlQuery = useSqlInspection(snapshot.id, true);
  const sqlTables =
    sqlQuery.data?.state.phase === "ready"
      ? sqlQuery.data.state.report.tables
      : undefined;

  const restoreActive = hasActiveRestorePhase(snapshot);
  const inFlight = isInFlight(snapshot);
  const showProgress = inFlight;

  // Bridge the perceptual gap between "I clicked Restore" and the first SSE
  // phase event landing. Show a small banner for ~8s after the confirmed POST,
  // OR until a restore phase event lands and the progress card renders for
  // real, whichever comes first. The banner is derived (`!restoreActive`) so it
  // disappears the instant a real restore phase arrives; the timeout below only
  // clears the pending marker so a later restore can re-arm the banner.
  const showRestoreBanner = restoreRequestedAt !== null && !restoreActive;
  useEffect(() => {
    if (restoreRequestedAt === null) return;
    const id = window.setTimeout(() => setRestoreRequestedAt(null), 8000);
    return () => window.clearTimeout(id);
  }, [restoreRequestedAt]);

  const componentCount = entries.length;
  const sizeLabel = formatBytes(snapshot.total_size);
  const kindLabel =
    snapshot.kind === "full"
      ? "Full backup"
      : snapshot.kind === "db"
        ? "Database backup"
        : "Files backup";
  const finishedRel = relativeTime(snapshot.finished_at);
  const subline = [
    kindLabel,
    sizeLabel,
    `${componentCount.toLocaleString()} ${componentCount === 1 ? "component" : "components"}`,
    finishedRel
      ? `finished ${finishedRel}`
      : `created ${relativeTime(snapshot.created_at) ?? snapshot.created_at}`,
  ].join(" · ");

  const canPressRestore = snapshot.status === "completed";
  const restoreDisabledReason =
    snapshot.status === "running"
      ? "Restore is available once this backup finishes."
      : snapshot.status === "pending"
        ? "Restore is available once this backup finishes."
        : snapshot.status === "failed"
          ? "This snapshot failed and cannot be restored."
          : "";

  const restoreButton = canRestore ? (
    <span title={canPressRestore ? undefined : restoreDisabledReason}>
      <Button
        variant="destructive"
        onClick={() => setRestoreOpen(true)}
        disabled={!canPressRestore}
        aria-disabled={!canPressRestore}
        aria-label={
          canPressRestore ? "Restore site" : `Restore site (${restoreDisabledReason})`
        }
      >
        Restore site
      </Button>
    </span>
  ) : null;

  return (
    <div className="space-y-6">
      <PageHeader
        title={`Snapshot ${snapshot.id.slice(0, 8)}`}
        mono
        copyable={snapshot.id}
        badges={
          <span className="flex items-center gap-2">
            <KindBadge kind={snapshot.kind} />
            <StatusBadge status={snapshot.status} />
            {inFlight ? (
              <LiveIndicator state="connecting" label="In progress" />
            ) : null}
          </span>
        }
        subline={subline}
        actions={restoreButton}
        backTo={{
          to: "/sites/$siteId/backups",
          params: { siteId: snapshot.site_id },
          label: site?.name
            ? `Back to ${site.name} backups`
            : "Back to site backups",
        }}
      />

      {snapshot.status === "failed" && snapshot.error ? (
        <p
          role="alert"
          className="rounded-md border border-[var(--color-destructive)]/40 bg-destructive-subtle p-3 text-sm text-destructive-subtle-fg"
        >
          {snapshot.error}
        </p>
      ) : null}

      {showRestoreBanner ? (
        <p
          role="status"
          aria-live="polite"
          className="flex items-center gap-2 rounded-md border border-border bg-[var(--color-accent)] p-3 text-sm"
        >
          <LiveIndicator state="connecting" label="Restore requested" />
          <span className="text-muted-foreground">
            Waiting for the agent to acknowledge.
          </span>
        </p>
      ) : null}

      {/* In-flight hero: the progress card promoted to full-width at the top;
          everything below dims to keep attention on the live job. */}
      {showProgress ? <SnapshotProgressCard snapshot={snapshot} /> : null}

      <div
        className={cn(
          "grid gap-6 lg:grid-cols-[2fr_1fr]",
          showProgress && "opacity-60 transition-opacity",
        )}
        aria-hidden={showProgress ? true : undefined}
      >
        {/* Left (2/3) — Contents. */}
        <div className="space-y-6">
          <Card>
            <CardHeader>
              <CardTitle>Contents</CardTitle>
            </CardHeader>
            <CardContent className="space-y-5">
              <SqlInspectionCard snapshotId={snapshot.id} enabled bordered={false} />
              <ManifestCard entries={entries} sqlTables={sqlTables} />
            </CardContent>
          </Card>
        </div>

        {/* Right (1/3) — Provenance. */}
        <div className="space-y-6">
          <EnvironmentCard snapshotId={snapshot.id} />
          <Card>
            <CardHeader>
              <CardTitle>Storage</CardTitle>
            </CardHeader>
            <CardContent>
              <StorageFacts snapshot={snapshot} entryCount={entries.length} />
            </CardContent>
          </Card>
        </div>
      </div>

      {canRestore ? (
        <RestoreDialog
          open={restoreOpen}
          onClose={() => setRestoreOpen(false)}
          onRequested={() => setRestoreRequestedAt(Date.now())}
          onRestoreRunId={(runId) => {
            void navigate({
              to: "/restores/$restoreId",
              params: { restoreId: runId },
            });
          }}
          snapshotId={snapshot.id}
          entries={entries}
          siteHost={site ? hostOf(site.url) : undefined}
          snapshotTakenAt={snapshot.created_at}
          targetSiteUrl={site?.url}
        />
      ) : null}
    </div>
  );
}

/** Extract a bare host from a site URL for the destructive-confirm prompt. */
function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

function StorageFacts({
  snapshot,
  entryCount,
}: {
  snapshot: BackupSnapshot;
  entryCount: number;
}) {
  return (
    <DefinitionList
      rows={[
        { label: "Total size", value: formatBytes(snapshot.total_size), tabular: true },
        {
          label: "Chunks",
          value: snapshot.chunk_count?.toLocaleString(),
          tabular: true,
        },
        { label: "Components", value: entryCount.toLocaleString(), tabular: true },
        { label: "Archived", value: snapshot.archived ? "Yes" : "No" },
        { label: "Started", value: relativeTime(snapshot.started_at) },
        { label: "Finished", value: relativeTime(snapshot.finished_at) },
        ...(snapshot.age_recipient
          ? [{ label: "Encryption recipient", copyable: snapshot.age_recipient }]
          : [{ label: "Encryption recipient", value: undefined }]),
      ]}
    />
  );
}

// ADR-037 Sprint 1, 1D — environment fingerprint card. Old snapshots
// (pre-v0.9.10 agent) get a muted "not recorded" note instead of an error
// banner; the field is purely informational.
function EnvironmentCard({ snapshotId }: { snapshotId: string }) {
  const { data, isPending } = useSnapshotEnvironment(snapshotId);

  if (isPending) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Environment</CardTitle>
        </CardHeader>
        <CardContent className="space-y-2">
          <Skeleton className="h-4 w-3/4" />
          <Skeleton className="h-4 w-2/3" />
          <Skeleton className="h-4 w-1/2" />
        </CardContent>
      </Card>
    );
  }
  if (!data) return null;
  if (data.phase === "not_recorded") {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Environment</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted-foreground">
            Environment fingerprint not recorded for this snapshot. Rebuild with
            a v0.9.10+ agent to capture PHP, MySQL, and WordPress versions plus
            the table inventory at backup time.
          </p>
        </CardContent>
      </Card>
    );
  }
  if (data.phase === "unwired") {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Environment</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted-foreground">
            Environment fingerprint reader is not configured on this control
            plane.
          </p>
        </CardContent>
      </Card>
    );
  }
  if (data.phase === "error") {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Environment</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-destructive-subtle-fg">{data.message}</p>
        </CardContent>
      </Card>
    );
  }
  if (data.phase !== "ready") return null;
  return <EnvironmentReady env={data.env} />;
}

function EnvironmentReady({ env }: { env: EnvFingerprint }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Environment</CardTitle>
      </CardHeader>
      <CardContent>
        <DefinitionList
          rows={[
            { label: "WordPress", value: env.wp_version, mono: true },
            { label: "PHP", value: env.php_version, mono: true },
            { label: "MySQL", value: env.mysql_version, mono: true },
            { label: "Multisite", value: env.is_multisite ? "Yes" : "No" },
            { label: "Site URL", value: env.site_url, mono: true },
            { label: "Home URL", value: env.home_url, mono: true },
            { label: "Files", value: env.file_count.toLocaleString(), tabular: true },
            {
              label: "DB tables",
              value: env.db_table_count.toLocaleString(),
              tabular: true,
            },
            {
              label: "Plugins",
              value: env.plugin_slugs.length.toLocaleString(),
              tabular: true,
            },
            {
              label: "Themes",
              value: env.theme_slugs.length.toLocaleString(),
              tabular: true,
            },
            { label: "Total size", value: formatBytes(env.total_size_bytes), tabular: true },
            { label: "Schema version", value: String(env.schema_version), tabular: true },
            { label: "Captured", value: relativeTime(env.captured_at) },
          ]}
        />
      </CardContent>
    </Card>
  );
}
