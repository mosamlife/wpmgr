import { Link } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { planLabel } from "@/features/billing/plan-catalog";

// UpgradePrompt — the ONE consistent surface for the 402 site_limit_reached
// shape (see lib/api.ts's extractSiteLimitReached, the shared 402
// interceptor). Every site-create mutation error path that hits the hosted-
// billing site cap renders this, instead of a raw error string.
//
// Non-owners cannot act on billing (owner-only, mirrors the audit
// re-baseline gating) so they see a "ask your owner" line instead of a
// "View plans" button.

export interface UpgradePromptProps {
  open: boolean;
  onClose: () => void;
  limit: number;
  usage: number;
  plan: string;
  /** True only for an owner on a hosted instance — the only principal who can act. */
  canUpgrade: boolean;
}

export function UpgradePrompt({
  open,
  onClose,
  limit,
  usage,
  plan,
  canUpgrade,
}: UpgradePromptProps) {
  return (
    <Dialog open={open} onClose={onClose}>
      {open ? (
        <DialogContent ariaLabelledBy="upgrade-prompt-title">
          <DialogHeader>
            <DialogTitle id="upgrade-prompt-title">
              Site limit reached
            </DialogTitle>
          </DialogHeader>

          <DialogBody>
            <p className="text-sm text-[var(--color-foreground)]">
              Your {planLabel(plan)} plan includes {limit}{" "}
              {limit === 1 ? "site" : "sites"} and you&apos;re using {usage}.
              Upgrade to add more.
            </p>
            {!canUpgrade ? (
              <p className="text-sm text-[var(--color-muted-foreground)]">
                Ask your organisation owner to upgrade.
              </p>
            ) : null}
          </DialogBody>

          <DialogFooter className="pt-2">
            <Button type="button" variant="outline" onClick={onClose}>
              Close
            </Button>
            {canUpgrade ? (
              <Button asChild onClick={onClose}>
                <Link to="/settings/billing">View plans</Link>
              </Button>
            ) : null}
          </DialogFooter>
        </DialogContent>
      ) : null}
    </Dialog>
  );
}
