// Destinations list (ADR-036 P1). Renders the configured destinations for a
// site with edit/delete affordances and an inline "Add destination" form
// toggle. The default destination wears a chip; per-row buttons trigger the
// form modal / destructive confirm dialog.

import { useState } from "react";
import { Trash2, Pencil, Plus, Server, HardDrive, Cloud } from "lucide-react";

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
  if (kind === "local") return <HardDrive aria-hidden className="size-4" />;
  if (kind === "s3_compat") return <Cloud aria-hidden className="size-4" />;
  return <Server aria-hidden className="size-4" />;
}

export interface DestinationsListProps {
  siteId: string;
}

export function DestinationsList({ siteId }: DestinationsListProps) {
  const { data, isPending, isError, error, refetch } = useDestinations(siteId);
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
    <section aria-labelledby="destinations-heading" className="space-y-6">
      <div className="flex items-end justify-between gap-3">
        <div>
          <h2 id="destinations-heading" className="text-xl font-semibold">
            Backup destinations
          </h2>
          <p className="text-sm text-[var(--color-muted-foreground)]">
            Where this site&apos;s backup chunks should be stored.
          </p>
        </div>
        {!adding && !editing ? (
          <Button onClick={() => setAdding(true)}>
            <Plus aria-hidden className="size-4" /> Add destination
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
        <div role="alert" className="space-y-3">
          <p className="text-[var(--color-destructive)]">
            Failed to load destinations: {error.message}
          </p>
          <Button variant="outline" size="sm" onClick={() => void refetch()}>
            Retry
          </Button>
        </div>
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
                <TableHead>Where</TableHead>
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
                        <Badge variant="default">Default</Badge>
                      ) : null}
                    </div>
                  </TableCell>
                  <TableCell>{KIND_LABEL[d.kind]}</TableCell>
                  <TableCell className="text-sm text-[var(--color-muted-foreground)]">
                    {d.kind === "s3_compat"
                      ? `${d.bucket}${d.path_prefix ? `/${d.path_prefix}` : ""}`
                      : d.kind === "local"
                        ? "wp-content/wpmgr-backups"
                        : "WPMgr managed storage"}
                  </TableCell>
                  <TableCell>
                    {d.kind === "s3_compat" && !d.has_secret ? (
                      <span className="text-[var(--color-destructive)] text-sm">
                        Secret missing
                      </span>
                    ) : (
                      <span className="text-sm">Ready</span>
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex items-center justify-end gap-2">
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => {
                          setAdding(false);
                          setEditing(d);
                        }}
                        aria-label={`Edit ${d.label}`}
                      >
                        <Pencil aria-hidden className="size-4" /> Edit
                      </Button>
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => setDeleteTarget(d)}
                        aria-label={`Delete ${d.label}`}
                      >
                        <Trash2 aria-hidden className="size-4" /> Delete
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
    <div className="rounded-xl border border-dashed border-[var(--color-border)] p-8 text-center">
      <p className="text-sm">
        No destinations yet. WPMgr ships backups to our managed storage by
        default. Add a destination here if you want to send them elsewhere too.
      </p>
    </div>
  );
}
