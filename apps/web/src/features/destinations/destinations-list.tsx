// Destinations list (ADR-036 P1). Renders the configured destinations for a
// site with edit/delete affordances and an inline "Add destination" form
// toggle. The default destination wears a chip; per-row buttons trigger the
// form inline / destructive confirm dialog.

import { useState } from "react";
import { Trash2, Pencil, Plus, Server, HardDrive, Cloud, Database } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { PageError } from "@/components/feedback";
import { CopyableMono } from "@/components/shared/copyable-mono";
import { StatusChip } from "@/components/status";
import type { SiteDestination, SiteDestinationKind } from "@wpmgr/api";

import {
  useDestinations,
  useDeleteDestination,
} from "./use-destinations";
import { DestinationForm } from "./destination-form";

const KIND_LABEL: Record<SiteDestinationKind, string> = {
  cp: "CP storage",
  local: "Local folder",
  s3_compat: "S3-compatible",
};

function KindIcon({ kind }: { kind: SiteDestinationKind }) {
  if (kind === "local") return <HardDrive aria-hidden="true" className="size-4" />;
  if (kind === "s3_compat") return <Cloud aria-hidden="true" className="size-4" />;
  return <Server aria-hidden="true" className="size-4" />;
}

/** Human-readable location summary for the "Where" column. */
function whereFor(d: SiteDestination): React.ReactNode {
  if (d.kind === "s3_compat") {
    const path = d.bucket
      ? `${d.bucket}${d.path_prefix ? `/${d.path_prefix}` : ""}`
      : null;
    return path ? <CopyableMono value={path} label={`Copy bucket path for ${d.label}`} /> : null;
  }
  if (d.kind === "local") {
    return (
      <CopyableMono
        value="wp-content/wpmgr-backups"
        label={`Copy path for ${d.label}`}
      />
    );
  }
  return <span className="text-[var(--color-muted-foreground)]">WPMgr managed storage</span>;
}

export interface DestinationsListProps {
  siteId: string;
}

export function DestinationsList({ siteId }: DestinationsListProps) {
  const {
    data,
    isPending,
    isError,
    error,
    refetch,
    isRefetching,
  } = useDestinations(siteId);
  const del = useDeleteDestination(siteId);

  const [editing, setEditing] = useState<SiteDestination | null>(null);
  const [adding, setAdding] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<SiteDestination | null>(null);

  async function performDelete() {
    if (!deleteTarget) return;
    try {
      await del.mutateAsync(deleteTarget.id);
      setDeleteTarget(null);
    } catch {
      // Error surfaces inside the dialog body via the mutation state.
    }
  }

  return (
    <section aria-labelledby="destinations-list-heading" className="space-y-6">
      <div className="flex items-center justify-between gap-3">
        <h2 id="destinations-list-heading" className="text-base font-semibold">
          Destinations for this site
        </h2>
        {!adding && !editing ? (
          <Button
            type="button"
            size="sm"
            onClick={() => setAdding(true)}
          >
            <Plus aria-hidden="true" className="size-4" />
            Add destination
          </Button>
        ) : null}
      </div>

      {(adding || editing) ? (
        <div className="rounded-xl border border-[var(--color-border)] p-4">
          <DestinationForm
            siteId={siteId}
            initial={editing ?? undefined}
            onSaved={() => {
              setAdding(false);
              setEditing(null);
            }}
            onCancel={() => {
              setAdding(false);
              setEditing(null);
            }}
          />
        </div>
      ) : null}

      {isPending ? (
        <p role="status" className="text-[var(--color-muted-foreground)]">
          Loading destinations…
        </p>
      ) : isError ? (
        <PageError
          what="Could not load destinations."
          why={error.message}
          onRetry={() => void refetch()}
          retryLabel="Reload destinations"
          isRetrying={isRefetching}
        />
      ) : data.length === 0 ? (
        <EmptyState />
      ) : (
        <div className="rounded-xl border border-[var(--color-border)]">
          <Table>
            <caption className="sr-only">Configured backup destinations</caption>
            <TableHeader>
              <TableRow>
                <TableHead>Destination</TableHead>
                <TableHead>Kind</TableHead>
                <TableHead>Location</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="sr-only">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.map((d) => (
                <TableRow key={d.id}>
                  <TableCell className="font-medium">
                    <div className="flex items-center gap-2">
                      <KindIcon kind={d.kind} />
                      <span>{d.label}</span>
                      {d.is_default ? (
                        <Badge variant="secondary">Default</Badge>
                      ) : null}
                    </div>
                  </TableCell>
                  <TableCell className="text-sm">{KIND_LABEL[d.kind]}</TableCell>
                  <TableCell className="text-sm">
                    {whereFor(d)}
                  </TableCell>
                  <TableCell>
                    {d.kind === "s3_compat" && !d.has_secret ? (
                      <StatusChip tone="destructive" label="Secret missing" />
                    ) : (
                      <StatusChip tone="success" label="Ready" />
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex items-center justify-end gap-2">
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={() => {
                          setAdding(false);
                          setEditing(d);
                        }}
                        aria-label={`Edit ${d.label}`}
                      >
                        <Pencil aria-hidden="true" className="size-4" />
                        Edit
                      </Button>
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={() => setDeleteTarget(d)}
                        aria-label={`Delete ${d.label}`}
                      >
                        <Trash2 aria-hidden="true" className="size-4" />
                        Delete
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <DestructiveConfirm
        open={deleteTarget !== null}
        onClose={() => setDeleteTarget(null)}
        onConfirm={performDelete}
        title={`Delete destination "${deleteTarget?.label ?? ""}"`}
        consequencesBody={
          <div className="space-y-2">
            <p>
              Future backups for this site will fall back to the default
              destination. Existing backup chunks remain at the destination
              location; they are NOT moved or deleted.
            </p>
          </div>
        }
        resourceName={deleteTarget?.label ?? ""}
        confirmLabel="Delete destination"
        cancelLabel="Keep destination"
        isPending={del.isPending}
        errorMessage={del.isError ? del.error.message : null}
      />
    </section>
  );
}

function EmptyState() {
  return (
    <div
      role="status"
      aria-label="No destinations configured"
      className="flex flex-col items-center gap-3 rounded-xl border border-dashed border-[var(--color-border)] py-12 text-center"
    >
      <Database
        aria-hidden="true"
        strokeWidth={1.5}
        className="size-8 text-[var(--color-muted-foreground)]/50"
      />
      <div className="space-y-1">
        <p className="text-balance text-sm font-medium text-[var(--color-foreground)]">
          No destinations configured.
        </p>
        <p className="text-balance text-sm text-[var(--color-muted-foreground)]">
          WPMgr ships backups to managed storage by default. Add a destination
          to send them elsewhere too.
        </p>
      </div>
    </div>
  );
}
