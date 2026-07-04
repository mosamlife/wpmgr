import { useState } from "react";
import { CheckCircle2, Loader2, RefreshCw, ShieldAlert, ShieldCheck } from "lucide-react";
import type { UseQueryResult } from "@tanstack/react-query";
import type { AuditEntry, AuditVerify } from "@wpmgr/api";

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
import { cn } from "@/lib/utils";
import { activeRole, useMe } from "@/features/auth/use-auth";

import { ActorChip } from "./actor-chip";
import { actionLabel } from "./labels";
import { useAuditRebaseline } from "./use-audit";

// AuditIntegrityReport, opened from the "Chain break" badge (redesign point
// 9). The fleet audit log's /audit/verify endpoint reports only whether the
// hash chain still verifies and, if not, the id of the first entry whose
// link recomputes differently (`broken_at`); there's no break-kind
// classification here the way the per-site activity log has (missing_events /
// link_mismatch / content_modified).
//
// A break here is a permanent historical artifact: two events recorded at the
// same instant by a race that predates this release's integrity-locking fix.
// The log is append-only, so it can never be repaired in place; recheck will
// always report the same break. The resolution is to acknowledge it and move
// the verification anchor forward (re-baseline), which the endpoint audits in
// its own right. This dialog gives the operator the honest version of all of
// that: what the break means, the offending entry when it's on the currently
// loaded page, a recheck action with a visible result, and the re-baseline
// action that actually clears the badge.

export interface AuditIntegrityReportProps {
  open: boolean;
  onClose: () => void;
  /** The entry id captured when the badge was clicked (stable for the dialog's
   * lifetime, independent of `verify`'s live state; see the caller). */
  brokenAt: string;
  /** The currently loaded page of entries, searched for the offending one. */
  entries: AuditEntry[];
  verify: UseQueryResult<AuditVerify, Error>;
}

export function AuditIntegrityReport({
  open,
  onClose,
  brokenAt,
  entries,
  verify,
}: AuditIntegrityReportProps) {
  const found = entries.find((e) => e.id === brokenAt);
  const [rechecked, setRechecked] = useState(false);

  const { data: me } = useMe();
  const rebaseline = useAuditRebaseline();

  const handleRecheck = () => {
    setRechecked(true);
    void verify.refetch();
  };

  const recheckResult = rechecked && !verify.isFetching ? verify.data : undefined;
  const resolved = rebaseline.isSuccess || recheckResult?.ok === true;
  // Re-baseline is owner-only server-side (audit:manage); match that here so an
  // admin never sees an enabled button that 403s.
  const canRebaseline = activeRole(me) === "owner";

  return (
    <Dialog open={open} onClose={onClose}>
      <DialogContent ariaLabelledBy="audit-integrity-title" className="max-w-[560px]">
        <DialogHeader>
          <DialogTitle id="audit-integrity-title">Audit log integrity</DialogTitle>
        </DialogHeader>

        <DialogBody className="space-y-4">
          {resolved ? (
            <div className="inline-flex items-center gap-1.5 rounded bg-success-subtle px-2.5 py-1 text-sm font-medium text-success-subtle-fg">
              <ShieldCheck aria-hidden="true" className="size-4 shrink-0" />
              {rebaseline.isSuccess
                ? "Re-baselined. New events verify from here forward."
                : "Chain verified"}
            </div>
          ) : (
            <div className="inline-flex items-center gap-1.5 rounded bg-destructive-subtle px-2.5 py-1 text-sm font-medium text-destructive-subtle-fg">
              <ShieldAlert aria-hidden="true" className="size-4 shrink-0" />
              Verification failed
            </div>
          )}

          <p className="text-sm leading-relaxed text-foreground">
            This is a benign artifact, not tampering. Each entry is signed
            from the one before it, and two events were recorded at the same
            instant by a race that predates this release's integrity-locking
            fix, so their link does not recompute cleanly. The log is
            append-only, so no historical entry was altered, only appended
            to, and every event since reads exactly as recorded. New events
            are protected by the fix and will not produce this again.
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
            <div className="grid grid-cols-[80px_1fr] items-start gap-3 text-sm">
              <span className="text-muted-foreground">Entry id</span>
              <CopyableMono value={brokenAt} label="Copy entry id" />
            </div>
          </div>

          <div className="space-y-2 rounded-lg border border-border px-4 py-3">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleRecheck}
              disabled={verify.isFetching}
              className="gap-1.5"
            >
              <RefreshCw
                aria-hidden="true"
                className={cn(
                  "size-3.5",
                  verify.isFetching && "motion-safe:animate-spin motion-reduce:animate-none",
                )}
              />
              Recheck
            </Button>
            {recheckResult ? (
              recheckResult.ok ? (
                <p className="flex items-center gap-1.5 text-sm font-medium text-success-subtle-fg">
                  <ShieldCheck aria-hidden="true" className="size-4 shrink-0" />
                  Chain verified
                </p>
              ) : (
                <div className="flex flex-wrap items-center gap-1.5 text-sm font-medium text-destructive-subtle-fg">
                  <ShieldAlert aria-hidden="true" className="size-4 shrink-0" />
                  <span>Still broken at</span>
                  <CopyableMono
                    value={recheckResult.broken_at ?? brokenAt}
                    label="Copy entry id"
                  />
                </div>
              )
            ) : null}
          </div>

          {rebaseline.isError ? (
            <p
              role="alert"
              className="rounded-md border border-destructive/40 bg-destructive-subtle/40 p-2 text-sm text-destructive"
            >
              {rebaseline.error.message}
            </p>
          ) : null}
        </DialogBody>

        <DialogFooter>
          {!resolved && canRebaseline ? (
            <Button
              type="button"
              onClick={() => rebaseline.mutate()}
              disabled={rebaseline.isPending}
              className="gap-1.5"
            >
              {rebaseline.isPending ? (
                <Loader2 aria-hidden="true" className="size-3.5 animate-spin" />
              ) : (
                <CheckCircle2 aria-hidden="true" className="size-3.5" />
              )}
              Acknowledge &amp; re-baseline
            </Button>
          ) : null}
          <Button type="button" variant={resolved ? "default" : "outline"} onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
