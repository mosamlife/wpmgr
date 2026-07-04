import { ShieldAlert } from "lucide-react";
import type { AuditEntry } from "@wpmgr/api";

import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { CopyableMono } from "@/components/shared/copyable-mono";

import { ActorChip } from "./actor-chip";
import { actionLabel } from "./labels";

// AuditIntegrityReport — opens from the "Chain break" badge (redesign point
// 9). The fleet audit log's /audit/verify endpoint reports only whether the
// hash chain still verifies and, if not, the id of the first entry whose
// link recomputes differently (`broken_at`) — there's no break-kind
// classification here the way the per-site activity log has (missing_events /
// link_mismatch / content_modified). This dialog gives the operator the
// honest version of that: what a break means, the offending entry when it's
// on the currently loaded page, and the raw id to look up otherwise, plus a
// re-check action — rather than the previous dead pill that just named a
// raw UUID with nowhere to go.

export interface AuditIntegrityReportProps {
  open: boolean;
  onClose: () => void;
  brokenAt: string;
  /** The currently loaded page of entries, searched for the offending one. */
  entries: AuditEntry[];
  onRecheck: () => void;
}

export function AuditIntegrityReport({
  open,
  onClose,
  brokenAt,
  entries,
  onRecheck,
}: AuditIntegrityReportProps) {
  const found = entries.find((e) => e.id === brokenAt);

  return (
    <Dialog open={open} onClose={onClose}>
      <DialogContent ariaLabelledBy="audit-integrity-title" className="max-w-[560px]">
        <DialogHeader>
          <DialogTitle id="audit-integrity-title">Audit log integrity</DialogTitle>
        </DialogHeader>

        <DialogBody className="space-y-4">
          <div className="inline-flex items-center gap-1.5 rounded bg-destructive-subtle px-2.5 py-1 text-sm font-medium text-destructive-subtle-fg">
            <ShieldAlert aria-hidden="true" className="size-4 shrink-0" />
            Verification failed
          </div>

          <p className="text-sm leading-relaxed text-foreground">
            Each entry in this account&apos;s audit log is signed from the one
            before it, so any entry that was inserted, removed, or edited
            after the fact breaks every link that follows it. The most common
            cause is a database-level change made outside the normal write
            path (a restore, a manual cleanup) rather than proof of
            tampering, but it is worth confirming.
          </p>

          <div className="space-y-2">
            <h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
              Broken at
            </h3>
            {found ? (
              <div className="space-y-2 rounded-lg border border-border px-4 py-3">
                <p className="text-sm text-foreground">{actionLabel(found.action)}</p>
                <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                  <ActorChip entry={found} />
                  <span>{new Date(found.created_at).toLocaleString()}</span>
                </div>
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">
                This entry is not on the page currently loaded. Use the id
                below with an earlier page, or look it up directly.
              </p>
            )}
          </div>

          <div className="grid grid-cols-[120px_1fr] items-start gap-3 text-sm">
            <span className="text-muted-foreground">Entry id</span>
            <CopyableMono value={brokenAt} label="Copy entry id" />
          </div>
        </DialogBody>

        <DialogFooter>
          <Button type="button" variant="ghost" onClick={onRecheck}>
            Re-check
          </Button>
          <Button type="button" onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
