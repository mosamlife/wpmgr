import { useState } from "react";
import { AlertTriangle } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

import type { ResolvedSiteScope } from "./site-scope";
import {
  ENFORCEMENT_ALL_MODE_SENTENCE,
  ENFORCEMENT_CHECK_SENTENCE,
  ENFORCEMENT_LIST_MODE_SENTENCE,
  ENFORCEMENT_TAG_DRIFT_SENTENCE,
  HOW_WE_CHECK_AUDIT_PATH,
  HOW_WE_CHECK_HEADING,
  HOW_WE_CHECK_MECHANISM,
  HOW_WE_CHECK_REMEDY,
  HOW_WE_CHECK_SCOPE,
  HOW_WE_CHECK_TITLE,
  describeRefusals,
  type RefusalsSummary,
} from "./site-enforcement";

// Screen 8 (wireframes.html#s8) — "How site access is enforced".
//
// THREE STATES, ONE STRING SET, per the wireframe's own framing. Named-sites
// and tag-mode share the neutral box and its opening sentence; every-site
// REPLACES the box with a warning-toned one rather than decorating the same
// box, because "nothing is checked" is a materially different fact from "this
// list is checked" and dressing one as the other would be the exact collapse
// this screen exists to prevent.
//
// `refusals` IS OPTIONAL AND DELIBERATELY OMITTED BY EVERY CALLER TODAY. No
// endpoint on the control plane counts refusals per connection
// (use-ai-connections.ts's wire schema carries only `site_scope_mode`), and
// the one caller wired so far (the consent screen) has no connection id yet
// at all -- approval has not happened, so there is no history to have an
// opinion about, "not tracked yet" included. Passing `refusals` renders the
// explicit not-a-count sentence from site-enforcement.ts; omitting it leaves
// the block out entirely. Both are honest; which one applies depends on
// whether a connection exists to ask about.
export interface SiteEnforcementBoxProps {
  readonly scope: ResolvedSiteScope;
  readonly refusals?: RefusalsSummary;
}

export function SiteEnforcementBox({ scope, refusals }: SiteEnforcementBoxProps) {
  const [dialogOpen, setDialogOpen] = useState(false);

  // 'none' and 'unresolved' have no enforcement story of their own: the
  // site-scope summary above this box already says there is nothing to
  // check (no-selection/no-matches) or that the check cannot be described
  // yet (loading/failed). Rendering this box for either would either repeat
  // that sentence in different words or assert a mechanism over a scope we
  // do not yet know.
  if (scope.kind === "none" || scope.kind === "unresolved") return null;

  const isEveryMode = scope.kind === "all";

  return (
    <div className="mt-4">
      <div
        data-testid="site-enforcement-box"
        data-mode={isEveryMode ? "all" : scope.basis}
        className={
          isEveryMode
            ? "rounded-md border border-[var(--color-warning)]/40 bg-warning-subtle p-3 text-sm text-warning-subtle-fg"
            : "rounded-md border border-[var(--color-border)] p-3 text-sm text-[var(--color-foreground)]"
        }
      >
        <p className="font-medium">
          {isEveryMode ? (
            <>
              <AlertTriangle aria-hidden="true" className="mr-1.5 inline size-4 align-text-bottom" />
              Every site in this organisation
            </>
          ) : (
            "How site access is enforced"
          )}
        </p>

        {isEveryMode ? (
          <p className="mt-2">{ENFORCEMENT_ALL_MODE_SENTENCE}</p>
        ) : (
          <>
            <p className="mt-2 text-[var(--color-muted-foreground)]">{ENFORCEMENT_CHECK_SENTENCE}</p>
            <p className="mt-2 text-[var(--color-muted-foreground)]">
              {scope.basis === "tags" ? ENFORCEMENT_TAG_DRIFT_SENTENCE : ENFORCEMENT_LIST_MODE_SENTENCE}
            </p>
          </>
        )}

        {refusals !== undefined && (
          <p data-testid="site-enforcement-refusals" className="mt-2 text-[var(--color-muted-foreground)]">
            {describeRefusals(refusals)}
          </p>
        )}

        {!isEveryMode && (
          <p className="mt-2 text-right">
            <Button
              type="button"
              variant="link"
              size="sm"
              className="h-auto p-0"
              onClick={() => setDialogOpen(true)}
            >
              How we check this →
            </Button>
          </p>
        )}
      </div>

      <HowWeCheckDialog open={dialogOpen} onClose={() => setDialogOpen(false)} />
    </div>
  );
}

/**
 * The "How we check this" dialog (wireframes.html:2898-2922). Its job is to
 * say plainly that scoping is a request-time check the application makes, not
 * a boundary the database enforces, and to hand over the stronger lever
 * (pausing the assistant, or removing the site from every list) rather than
 * asking anyone to evaluate a threat model.
 */
export function HowWeCheckDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  return (
    <Dialog open={open} onClose={onClose}>
      <DialogContent ariaLabelledBy="how-we-check-title" className="max-w-[480px]">
        <DialogHeader>
          <DialogTitle id="how-we-check-title">{HOW_WE_CHECK_TITLE}</DialogTitle>
        </DialogHeader>
        <DialogBody className="text-sm text-[var(--color-muted-foreground)]">
          <p>{HOW_WE_CHECK_MECHANISM}</p>
          <p>{HOW_WE_CHECK_SCOPE}</p>
          <p className="font-medium text-[var(--color-foreground)]">{HOW_WE_CHECK_HEADING}</p>
          <p>{HOW_WE_CHECK_REMEDY}</p>
          <p>
            You can see every refusal:{" "}
            <span className="font-mono text-[var(--color-foreground)]">{HOW_WE_CHECK_AUDIT_PATH}</span>
          </p>
        </DialogBody>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
